package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	ErrCustomerPaymentUnsupported    = errors.New("customer-present payment is not supported for this rail")
	ErrUsageNotCollectible           = errors.New("the usage order takes no collection now")
	ErrDefaultPaymentMethodRequired  = errors.New("no default payment method for the currency")
	ErrDefaultPaymentMethodInvalid   = errors.New("the default payment method cannot pay this order")
	ErrCollectionInProgress          = errors.New("a collection of the order is in progress")
	ErrCollectionOutcomeUnknown      = errors.New("the order's collection outcome is unknown; resolve the operation before another attempt")
	ErrCollectionIdempotencyConflict = errors.New("the key names another collection")
)

// UsageCollectionRequest charges a usage order to one of its customer's
// saved cards; one IdempotencyKey is one collection.
type UsageCollectionRequest struct {
	OrderID         uuid.UUID
	IdempotencyKey  string
	PaymentMethodID uuid.UUID
}

// UsageCollection is a collection run for a request: its operation, and
// whether the key's first request started it.
type UsageCollection struct {
	Replayed  bool
	Operation gen.BillingProviderIntent
}

// DefaultPaymentMethodCurrencies is, per card of one customer, the currencies
// it is their default for.
func (s *MoneyService) DefaultPaymentMethodCurrencies(ctx context.Context, payer identity.CustomerID) (map[uuid.UUID][]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListMoneyAccountSettingsByCustomer(ctx, gen.ListMoneyAccountSettingsByCustomerParams{MerchantID: mid.UUID(), CustomerID: payer.UUID()})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID][]string)
	for _, row := range rows {
		if id := row.DefaultPaymentMethodID; id != nil {
			out[*id] = append(out[*id], row.Currency)
		}
	}
	return out, nil
}

// ChargeOutstanding collects the usage orders due for automatic collection by
// enqueuing and executing one durable order_collection operation per order.
// minThreshold > 0 charges only orders with at least that much due. Returns
// the number of settled collections.
func (s *MoneyService) ChargeOutstanding(ctx context.Context, runner *intents.Runner, minThreshold int64) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("money service not initialized")
	}
	if runner == nil {
		return 0, fmt.Errorf("intent runner required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Gen(ctx).ListChargeableUsageOrders(ctx, gen.ListChargeableUsageOrdersParams{MerchantID: tid.UUID(), Now: s.now(), MinThreshold: minThreshold})
	if err != nil {
		return 0, err
	}
	// One order's failure must not abort the rest of the merchant's batch.
	count := 0
	var errs []error
	for _, r := range rows {
		intentID, _, err := s.enqueueUsageCollection(ctx, identity.CustomerID(r.CustomerID), r.ID, usageCollectionEnqueue{minThreshold: minThreshold, origin: intents.OriginSystem, originReason: "scheduled usage collection"})
		if err != nil {
			errs = append(errs, fmt.Errorf("claim order %s: %w", r.ID, err))
			continue
		}
		if intentID == uuid.Nil {
			continue
		}
		row, err := runner.ExecuteByID(ctx, intentID)
		if err != nil {
			errs = append(errs, fmt.Errorf("collect order %s: %w", r.ID, err))
			continue
		}
		if row.Status == intents.StatusSucceeded {
			count++
		}
	}
	return count, errors.Join(errs...)
}

// ChargeMonthlyOutstanding scans one fixed thirty-day period per merchant.
// The marker belongs to the billing book, not River's prunable job history.
// Concurrent scans share the normal collection admission locks and receipts; no
// database transaction stays open across a provider request.
func (s *MoneyService) ChargeMonthlyOutstanding(ctx context.Context, runner *intents.Runner, minThreshold int64) (int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	pool := s.db.Pool()
	if pool == nil {
		return 0, errors.New("monthly usage collection requires its coordination pool")
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("acquire monthly usage coordination connection: %w", err)
	}
	defer conn.Release()
	coordination := gen.New(conn)
	held, err := coordination.TryLockUsageMonthlyCollection(ctx, mid.UUID())
	if err != nil {
		return 0, fmt.Errorf("lock monthly usage collection: %w", err)
	}
	if !held {
		return 0, errors.New("monthly usage collection is already running")
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := coordination.UnlockUsageMonthlyCollection(unlockCtx, mid.UUID()); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	period := s.now().UTC().Truncate(30 * 24 * time.Hour)
	last, err := s.db.Gen(ctx).GetUsageMonthlyCollectionPeriod(ctx, mid.UUID())
	if err == nil && !last.Before(period) {
		return 0, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("read monthly usage cadence: %w", err)
	}
	// ListChargeableUsageOrders scans every eligible row without a page limit.
	// Partial admission errors leave the period open; accepted intents already
	// own any submitted or deferred payments and recover independently.
	count, err := s.ChargeOutstanding(ctx, runner, minThreshold)
	if err != nil {
		return count, err
	}
	if err := conn.Ping(ctx); err != nil {
		return count, fmt.Errorf("monthly usage coordination connection was lost: %w", err)
	}
	if err := s.db.Gen(ctx).CompleteUsageMonthlyCollectionPeriod(ctx, gen.CompleteUsageMonthlyCollectionPeriodParams{
		MerchantID: mid.UUID(), MonthlyPeriodStartedAt: period, CompletedAt: s.now().UTC(),
	}); err != nil {
		return count, fmt.Errorf("complete monthly usage cadence: %w", err)
	}
	return count, nil
}

// PayUsageNow is the customer paying their usage order with a saved card,
// in person. It is called only by the verified-customer command; its
// customer-present posture is frozen in the accepted operation.
func (s *MoneyService) PayUsageNow(ctx context.Context, runner *intents.Runner, principal billingauth.Payer, request UsageCollectionRequest) (*UsageCollection, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	payer, err := billing.ParseCustomerID(principal.SubjectID)
	if err != nil || payer.IsZero() || billingauth.ValidatePayer(&principal) != nil || principal.CredentialClass != billingauth.CredentialClassUserSession || principal.Invoker != "" || principal.MerchantID != mid {
		return nil, ErrCustomerSessionRequired
	}
	return s.collectUsage(ctx, runner, payer, request)
}

func (s *MoneyService) collectUsage(ctx context.Context, runner *intents.Runner, payer identity.CustomerID, request UsageCollectionRequest) (*UsageCollection, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if runner == nil {
		return nil, fmt.Errorf("intent runner required")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if request.OrderID == uuid.Nil || request.PaymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("order and payment method required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be between 1 and 255 bytes")
	}
	key := charge.CustomerPaymentKey(TypeOrderCollection, payer.UUID(), request.IdempotencyKey)
	pm := request.PaymentMethodID
	intentID, replayed, err := s.enqueueUsageCollection(ctx, payer, request.OrderID, usageCollectionEnqueue{
		manual: true, paymentMethodID: &pm, operationKey: key, origin: intents.OriginUser, originReason: "verified customer usage payment", initiator: charge.InitiatorCustomer,
	})
	if err != nil {
		return nil, err
	}
	if intentID == uuid.Nil {
		return nil, ErrUsageNotCollectible
	}
	operation, err := runner.ExecuteByID(ctx, intentID)
	if err != nil {
		return nil, fmt.Errorf("collect usage order: %w", err)
	}
	return &UsageCollection{Replayed: replayed, Operation: operation}, nil
}

func scheduledUsageCollectionKey(orderID uuid.UUID, ordinal int64) string {
	return fmt.Sprintf("%s:%s:attempt:%d", TypeOrderCollection, orderID, ordinal)
}

func scheduledUsageCollectionEligible(order gen.BillingOrder, minThreshold int64, now time.Time) bool {
	due := usage.Due(order)
	if order.CollectionOwner != string(billing.OrderCollectionEngine) || order.Status != string(billing.OrderOpen) || order.CollectionMethod == nil || *order.CollectionMethod != CollectionChargeAutomatically ||
		due <= 0 || (minThreshold > 0 && due < minThreshold) {
		return false
	}
	if order.DueAt != nil && order.DueAt.After(now) {
		return false
	}
	return derefInt32(order.CollectionFailureCount) == 0 && order.NextCollectionAttemptAt == nil ||
		order.NextCollectionAttemptAt != nil && !order.NextCollectionAttemptAt.After(now)
}

type usageCollectionEnqueue struct {
	initiator       charge.Initiator
	minThreshold    int64
	manual          bool
	paymentMethodID *uuid.UUID
	operationKey    string
	origin          intents.Origin
	originReason    string
}

// enqueueUsageCollection freezes one collection atomically: the intent and
// the order's live attempt commit together, or nothing does. A client key is
// looked up under the order lock, so two equal concurrent requests serialize
// on the row and the second replays the first's operation (replayed=true).
// intentID == Nil means the order was not eligible (scheduled path); the
// manual path reports why.
func (s *MoneyService) enqueueUsageCollection(ctx context.Context, payer identity.CustomerID, orderID uuid.UUID, opts usageCollectionEnqueue) (intentID uuid.UUID, replayed bool, err error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, false, err
	}
	now := s.now()
	if opts.initiator == "" {
		opts.initiator = charge.InitiatorMerchant
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: tid.UUID(), ID: payer.UUID()}); err != nil {
			return fmt.Errorf("lock usage payer: %w", err)
		}
		order, err := q.LockUsageOrder(ctx, gen.LockUsageOrderParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: orderID})
		if errors.Is(err, pgx.ErrNoRows) && !opts.manual {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock usage order: %w", err)
		}
		if order.CollectionOwner != string(billing.OrderCollectionEngine) {
			if opts.manual {
				return ErrUsageActionNotAllowed
			}
			return nil
		}
		if opts.operationKey != "" {
			prior, err := q.GetProviderIntentByIdempotencyKey(ctx, gen.GetProviderIntentByIdempotencyKeyParams{MerchantID: tid.UUID(), IdempotencyKey: opts.operationKey})
			switch {
			case err == nil:
				frozen, err := intents.DecodeOrderCollectionPayload(prior)
				if err != nil {
					return err
				}
				if frozen.OrderID != orderID || frozen.CustomerID != payer.UUID() || opts.paymentMethodID == nil || frozen.PaymentMethodID != *opts.paymentMethodID || frozen.Initiator != opts.initiator {
					return ErrCollectionIdempotencyConflict
				}
				intentID, replayed = prior.ID, true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("load collection operation: %w", err)
			}
		}
		if order.Status == string(billing.OrderProcessing) {
			if !opts.manual {
				return nil
			}
			if order.AttemptID != nil {
				live, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: tid.UUID(), ID: *order.AttemptID})
				if err == nil && live.Status == intents.StatusUnknownNeedsVerify {
					return ErrCollectionOutcomeUnknown
				}
			}
			return ErrCollectionInProgress
		}
		eligible := scheduledUsageCollectionEligible(order, opts.minThreshold, now)
		if opts.manual {
			eligible = opts.initiator == charge.InitiatorCustomer && usage.Due(order) > 0 &&
				(order.Status == string(billing.OrderOpen) || order.Status == string(billing.OrderUncollectible))
		}
		if !eligible {
			return nil
		}
		method, err := s.collectionMethodFor(ctx, q, tid.UUID(), payer.UUID(), order.Currency, opts)
		if errors.Is(err, charge.ErrAgreementRequired) && !opts.manual {
			return stopCollectionForAgreement(ctx, s.db.NewWithPgxTx(tx), order, now)
		}
		if err != nil || method == nil {
			return err
		}
		// The account the charge goes through: the card's own PSP, or the one
		// PSP of its rail that reaches its custodian.
		psp, err := charge.RoutePSP(ctx, charge.CustodyOf(s.db), *method)
		if err != nil {
			return fmt.Errorf("route order %s collection: %w", order.ID, err)
		}
		receipt, found, err := s.findObservedUsagePayment(ctx, q, tid.UUID(), order.ID, psp)
		if err != nil {
			return err
		}
		if found {
			local := NewMoneyService(s.db.NewWithPgxTx(tx), s.Clock())
			_, err := local.RecoverObservedUsagePayment(ctx, receipt)
			return err // committed readback; no new operation or invented initiator
		}
		// A recent read on the newly selected account cannot exclude a payment
		// through another known card account after the restored snapshot.
		accounts, err := q.UsageRecoveryAccounts(ctx, gen.UsageRecoveryAccountsParams{MerchantID: tid.UUID(), RoutedPsp: psp})
		if err != nil {
			return err
		}
		for _, account := range accounts {
			if err := providerrecovery.CheckPSP(ctx, s.db.NewWithPgxTx(tx), tid.UUID(), account.ID, now); err != nil {
				return err
			}
		}
		var custody *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			binding, err := charge.FreezeHyperSwitchBinding(ctx, q, charge.CustodyOf(s.db), *method, psp, s.hyperSwitchDeployment)
			if err != nil {
				return err
			}
			custody = &binding
		}
		instrument, err := collectionInstrument(ctx, q, *method, psp, order.Currency, opts.initiator)
		if err != nil {
			return err
		}
		due := usage.Due(order)
		amountMinor, err := moneyutil.NativeToRailMinor(order.Currency, due)
		if err != nil {
			return fmt.Errorf("order %s amount is not representable on rail %s: %w", order.ID, method.Rail, err)
		}
		providerCustomerRef := ""
		if normalizeRail(method.Rail) == "stripe" {
			providerCustomerRef, err = q.GetPSPCustomerRef(ctx, gen.GetPSPCustomerRefParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), PspID: psp})
			if err != nil {
				return fmt.Errorf("freeze Stripe customer on accepted account: %w", err)
			}
			if strings.TrimSpace(providerCustomerRef) == "" {
				return errors.New("Stripe collection customer mapping is empty")
			}
		}
		if _, err := moneyutil.RailMinorToNative(order.Currency, amountMinor); err != nil {
			return fmt.Errorf("order %s rounded charge is not representable: %w", order.ID, err)
		}
		key := opts.operationKey
		if key == "" {
			ordinal, err := q.CountOrderAttempts(ctx, gen.CountOrderAttemptsParams{MerchantID: tid.UUID(), OrderID: orderID})
			if err != nil {
				return err
			}
			key = scheduledUsageCollectionKey(orderID, ordinal)
		}
		paymentID := uuidutil.NewV7()
		intent, err := intents.NewStore(s.db.NewWithPgxTx(tx)).Enqueue(ctx, intents.EnqueueParams{
			MerchantID: tid.UUID(), Provider: normalizeRail(method.Rail), PspID: psp, IntentType: TypeOrderCollection,
			Payload: intents.OrderCollectionPayload{
				OrderID: orderID, CustomerID: payer.UUID(), PaymentID: paymentID, PaymentMethodID: method.ID, Initiator: opts.initiator,
				Rail: normalizeRail(method.Rail), Instrument: instrument,
				HyperSwitch: custody,
				Currency:    order.Currency, Amount: due, AmountMinor: amountMinor,
				ProviderCustomerRef: providerCustomerRef,
				// The provider's description names the order as the invoice it
				// renders; recovery finds a collection's sale by it.
				Description: fmt.Sprintf("invoice %s", orderID),
			},
			IdempotencyKey: key, NextAttemptAt: now, Origin: opts.origin, OriginReason: opts.originReason, Inline: true,
			Actor: func() string {
				if opts.initiator == charge.InitiatorCustomer {
					return payer.UUID().String()
				}
				return ""
			}(),
		})
		if err != nil {
			return fmt.Errorf("enqueue order collection: %w", err)
		}
		canonical, err := intents.DecodeOrderCollectionPayload(intent)
		if err != nil {
			return err
		}
		if canonical.OrderID != orderID || canonical.CustomerID != payer.UUID() || canonical.PaymentMethodID != method.ID || canonical.Initiator != opts.initiator {
			return ErrCollectionIdempotencyConflict
		}
		if canonical.PaymentID != paymentID {
			intentID, replayed = intent.ID, true
			return nil
		}
		if err := usage.StartCollection(ctx, q, order, intent.ID, psp, normalizeRail(method.Rail), method.ID, due, now); err != nil {
			return err
		}
		intentID = intent.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return intentID, replayed, nil
}

// collectionMethodFor resolves the payer-owned saved method the collection
// charges: the explicitly bound one (a retry or the customer's payment) or
// the customer's default for the currency. The account that vaulted the
// instrument takes the money. The row is read under a shared lock so the
// instrument the operation freezes cannot be remapped before it commits.
func (s *MoneyService) collectionMethodFor(ctx context.Context, q *gen.Queries, merchantID, payerID uuid.UUID, currency string, opts usageCollectionEnqueue) (*gen.BillingPaymentMethod, error) {
	id := opts.paymentMethodID
	if id == nil {
		settingsRow, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: merchantID, CustomerID: payerID, Currency: currency})
		if errors.Is(err, pgx.ErrNoRows) {
			if opts.manual {
				return nil, ErrDefaultPaymentMethodRequired
			}
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load collection settings: %w", err)
		}
		id = settingsRow.DefaultPaymentMethodID
		if id == nil {
			if opts.manual {
				return nil, ErrDefaultPaymentMethodRequired
			}
			return nil, nil
		}
	}
	method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: *id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDefaultPaymentMethodInvalid
		}
		return nil, fmt.Errorf("load collection payment method: %w", err)
	}
	if method.MerchantID != merchantID || method.CustomerID != payerID || !paymentmethods.Chargeable(method) {
		return nil, ErrDefaultPaymentMethodInvalid
	}
	if descriptor, ok := rails.Lookup(models.Rail(method.Rail)); !ok || !descriptor.SupportsChargeSavedMethod {
		return nil, ErrDefaultPaymentMethodInvalid
	}
	if opts.initiator == charge.InitiatorCustomer && (!rails.IsNMI(models.Rail(method.Rail)) || (method.Custodian != models.CustodianPSP && method.Custodian != models.CustodianHyperSwitch)) {
		return nil, ErrCustomerPaymentUnsupported
	}
	if opts.initiator == charge.InitiatorMerchant {
		if err := requireCollectionAgreement(ctx, q, charge.CustodyOf(s.db), method, currency); err != nil {
			if opts.manual && errors.Is(err, charge.ErrAgreementRequired) {
				return nil, fmt.Errorf("%w: %w", ErrDefaultPaymentMethodInvalid, err)
			}
			return nil, err
		}
	}
	return &method, nil
}

// requireCollectionAgreement refuses a merchant-initiated collection in
// currency on a card without the customer's active unscheduled mandate for it
// on the account that charges it: never given, or waiting for consent after
// the card was reissued under another brand.
func requireCollectionAgreement(ctx context.Context, q *gen.Queries, custody charge.Custody, method gen.BillingPaymentMethod, currency string) error {
	psp, err := charge.RoutePSP(ctx, custody, method)
	if err != nil {
		return err
	}
	_, err = mandates.ForCollection(ctx, q, method.MerchantID, method.CustomerID, currency, method.ID, psp)
	return err
}

// stopCollectionForAgreement stops collecting a usage order whose collection
// card carries no agreement for a merchant-initiated charge, and asks the
// customer to act. Designating a card resumes it.
func stopCollectionForAgreement(ctx context.Context, d *db.DB, order gen.BillingOrder, now time.Time) error {
	failure := billing.PaymentFailure{Reason: charge.AgreementRequiredCode, Message: charge.ErrAgreementRequired.Error()}
	if err := usage.StopCollection(ctx, d.Gen(ctx), order.MerchantID, order.ID, failure, now); err != nil {
		return err
	}
	action := collection.Action{Decline: decline.Result{Action: decline.FixPaymentMethod}}
	return queueCollectionOutcome(ctx, d, order, action, charge.AgreementRequiredCode, now)
}

// collectionInstrument freezes a collection's card and the mandate it cites:
// a merchant-initiated collection runs under the currency's active
// unscheduled mandate; a customer-present payment cites the card's
// unscheduled lineage, or stores it.
func collectionInstrument(ctx context.Context, q *gen.Queries, method gen.BillingPaymentMethod, psp uuid.UUID, currency string, initiator charge.Initiator) (charge.FrozenInstrument, error) {
	instrument := charge.FreezeInstrument(method, psp)
	var err error
	if initiator == charge.InitiatorMerchant {
		instrument.Mandate, err = mandates.ForCollection(ctx, q, method.MerchantID, method.CustomerID, currency, method.ID, psp)
		return instrument, err
	}
	instrument.Mandate, err = mandates.Citable(ctx, q, method.MerchantID, method.CustomerID, method.ID, psp, method.Rail, charge.AgreementCardOnFile)
	return instrument, err
}
