package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/mandates"
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
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/providerrecovery"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

var (
	ErrCustomerPaymentUnsupported      = errors.New("customer-present payment is not supported for this rail")
	ErrInvoiceNotRetryable             = errors.New("invoice is not retryable")
	ErrCollectionPaymentMethodRequired = errors.New("collection payment method required")
	ErrCollectionPaymentMethodInvalid  = errors.New("collection payment method invalid")
	ErrInvoiceRetryInProgress          = errors.New("invoice collection is in progress")
	ErrInvoiceRetryOutcomeUnknown      = errors.New("invoice collection outcome is unknown; resolve the operation before another attempt")
	ErrInvoiceRetryIdempotencyConflict = errors.New("invoice retry idempotency conflict")
)

type InvoiceCollectionRetryRequest struct {
	InvoiceID       uuid.UUID
	IdempotencyKey  string
	PaymentMethodID uuid.UUID
}

// InvoiceCollectionRetryResult is the invoice after its collection ran, with
// the payment when the charge settled.
type InvoiceCollectionRetryResult struct {
	Invoice   *models.Invoice
	Payment   *models.Payment
	Replayed  bool
	Operation gen.BillingProviderIntent
}

// collectionPaymentMethodID selects the explicit invoice collection method.
func collectionPaymentMethodID(settings *models.MoneyAccount) *uuid.UUID {
	return settings.CollectionPaymentMethod
}

// CollectionPaymentMethodCurrencies projects the current collection policy for
// one customer, with explicit currencies instead of a misleading universal default.
func (s *MoneyService) CollectionPaymentMethodCurrencies(ctx context.Context, payer identity.CustomerID) (map[uuid.UUID][]string, error) {
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
		if id := collectionPaymentMethodID(settingsFromGen(row)); id != nil {
			out[*id] = append(out[*id], row.Currency)
		}
	}
	return out, nil
}

// SetInvoiceCollectionPaymentMethod selects the payer-owned saved method used
// for automatic invoice collection in one billing currency.
func (s *MoneyService) SetInvoiceCollectionPaymentMethod(ctx context.Context, payer identity.CustomerID, currency string, paymentMethodID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return fmt.Errorf("payer required")
	}
	if paymentMethodID == uuid.Nil {
		return fmt.Errorf("payment_method_id required")
	}
	currency = normalizeCurrency(currency)
	if err := RequireBillingCurrency(currency); err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: tid.UUID(), ID: paymentMethodID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCollectionPaymentMethodInvalid
			}
			return fmt.Errorf("load collection payment method: %w", err)
		}
		if method.MerchantID != tid.UUID() || method.CustomerID != payer.UUID() {
			return ErrCollectionPaymentMethodInvalid
		}
		if strings.TrimSpace(models.DerefStr(method.ParkReason)) != "" {
			return fmt.Errorf("%w: payment method is parked", ErrCollectionPaymentMethodInvalid)
		}
		descriptor, ok := rails.Lookup(models.Rail(method.Rail))
		if !ok {
			return fmt.Errorf("%w: unknown rail %q", ErrCollectionPaymentMethodInvalid, method.Rail)
		}
		if !descriptor.SupportsChargeSavedMethod {
			return fmt.Errorf("%w: rail %q does not support invoice collection", ErrCollectionPaymentMethodInvalid, method.Rail)
		}
		psp, err := charge.RoutePSP(ctx, q, method)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCollectionPaymentMethodInvalid, err)
		}
		// The default carries the customer's consent to collection in this
		// currency: its unscheduled mandate cites the card's own lineage on
		// the account that charges it.
		lineage, err := mandates.Citable(ctx, q, tid.UUID(), payer.UUID(), method.ID, psp, method.Rail, charge.AgreementUnscheduled)
		if err != nil {
			return err
		}
		if lineage == nil {
			return fmt.Errorf("%w: automatic collection: %w", ErrCollectionPaymentMethodInvalid, charge.ErrAgreementRequired)
		}
		if err := s.ensureSettingsRowTx(ctx, q, tid.UUID(), payer.UUID(), currency, BillingModePrepaid, now); err != nil {
			return fmt.Errorf("ensure money account settings: %w", err)
		}
		n, err := q.SetMoneyAccountCollectionPaymentMethod(ctx, gen.SetMoneyAccountCollectionPaymentMethodParams{
			MerchantID:      tid.UUID(),
			CustomerID:      payer.UUID(),
			Currency:        currency,
			PaymentMethodID: &paymentMethodID,
			Now:             now,
		})
		if err != nil {
			return fmt.Errorf("set collection payment method: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("set collection payment method: settings row not found")
		}
		if _, err := mandates.Replace(ctx, q, mandates.Agreement{MerchantID: tid.UUID(), CustomerID: payer.UUID(), PaymentMethodID: method.ID, PSPID: psp, Rail: method.Rail,
			Kind: charge.AgreementUnscheduled, Currency: currency, Lineage: lineage, AcceptedAt: now}, now); err != nil {
			return err
		}
		// or#828 bucket-2 resume. Designating a collection payment method is
		// exactly the action the "update your payment method" notice asked for,
		// so every invoice of theirs that STOPPED for want of a working
		// instrument becomes due again now. Without this the bucket-2 stop is
		// still a state nothing resolves — the customer does what we asked and
		// nothing happens.
		resumed, err := q.ResumeStoppedInvoiceCollection(ctx, gen.ResumeStoppedInvoiceCollectionParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(), Currency: currency, Now: now,
		})
		if err != nil {
			return fmt.Errorf("resume stopped invoice collection: %w", err)
		}
		if resumed > 0 {
			log.WithContext(ctx).WithFields(log.Fields{
				"customer_id": payer.UUID(), "currency": currency, "invoices": resumed,
			}).Info("collection payment method set; stopped invoice collection resumed")
		}
		return nil
	})
}

// ChargeOutstanding collects open/past-due invoice receivables due for
// automatic collection by enqueuing and executing one durable
// invoice_collection operation per invoice. minThreshold > 0 charges only
// invoices due at least that much. Returns the number of settled invoices.
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
	rows, err := s.db.Gen(ctx).ListChargeableOpenInvoices(ctx, gen.ListChargeableOpenInvoicesParams{MerchantID: tid.UUID(), Now: s.now(), MinThreshold: minThreshold})
	if err != nil {
		return 0, err
	}
	// One invoice's failure must not abort the rest of the merchant's batch.
	count := 0
	var errs []error
	for _, r := range rows {
		intentID, _, err := s.enqueueInvoiceCollection(ctx, identity.CustomerID(r.CustomerID), r.ID, invoiceCollectionEnqueue{minThreshold: minThreshold, origin: intents.OriginSystem, originReason: "scheduled invoice collection"})
		if err != nil {
			errs = append(errs, fmt.Errorf("claim invoice %s: %w", r.ID, err))
			continue
		}
		if intentID == uuid.Nil {
			continue
		}
		row, err := runner.ExecuteByID(ctx, intentID)
		if err != nil {
			errs = append(errs, fmt.Errorf("collect invoice %s: %w", r.ID, err))
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
// Concurrent scans share the normal invoice admission locks and receipts; no
// database transaction stays open across a provider request.
func (s *MoneyService) ChargeMonthlyOutstanding(ctx context.Context, runner *intents.Runner, minThreshold int64) (int, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	pool := s.db.Pool()
	if pool == nil {
		return 0, errors.New("monthly invoice collection requires its coordination pool")
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, err := pool.Acquire(acquireCtx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("acquire monthly invoice coordination connection: %w", err)
	}
	defer conn.Release()
	coordination := gen.New(conn)
	held, err := coordination.TryLockInvoiceMonthlyCollection(ctx, mid.UUID())
	if err != nil {
		return 0, fmt.Errorf("lock monthly invoice collection: %w", err)
	}
	if !held {
		return 0, errors.New("monthly invoice collection is already running")
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := coordination.UnlockInvoiceMonthlyCollection(unlockCtx, mid.UUID()); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	period := s.now().UTC().Truncate(30 * 24 * time.Hour)
	last, err := s.db.Gen(ctx).GetInvoiceMonthlyCollectionPeriod(ctx, mid.UUID())
	if err == nil && !last.Before(period) {
		return 0, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("read monthly invoice cadence: %w", err)
	}
	// ListChargeableOpenInvoices scans every eligible row without a page limit.
	// Partial admission errors leave the period open; accepted intents already
	// own any submitted or deferred payments and recover independently.
	count, err := s.ChargeOutstanding(ctx, runner, minThreshold)
	if err != nil {
		return count, err
	}
	if err := conn.Ping(ctx); err != nil {
		return count, fmt.Errorf("monthly invoice coordination connection was lost: %w", err)
	}
	if err := s.db.Gen(ctx).CompleteInvoiceMonthlyCollectionPeriod(ctx, gen.CompleteInvoiceMonthlyCollectionPeriodParams{
		MerchantID: mid.UUID(), MonthlyPeriodStartedAt: period, CompletedAt: s.now().UTC(),
	}); err != nil {
		return count, fmt.Errorf("complete monthly invoice cadence: %w", err)
	}
	return count, nil
}

// RetryInvoiceCollection binds a client retry key to one saved payment method
// and runs the durable collection operation. Reusing the key returns that
// operation's durable state without another provider charge; a different
// method under the same key is a conflict.
func (s *MoneyService) RetryInvoiceCollection(ctx context.Context, runner *intents.Runner, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	return s.retryInvoiceCollection(ctx, runner, payer, request, charge.InitiatorMerchant)
}

// PayInvoiceNow is called only by the verified-payer command handler. Its
// customer-present posture is frozen in the accepted operation, not a body flag.
func (s *MoneyService) PayInvoiceNow(ctx context.Context, runner *intents.Runner, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
	return s.retryInvoiceCollection(ctx, runner, payer, request, charge.InitiatorCustomer)
}

func (s *MoneyService) retryInvoiceCollection(ctx context.Context, runner *intents.Runner, payer identity.CustomerID, request InvoiceCollectionRetryRequest, initiator charge.Initiator) (*InvoiceCollectionRetryResult, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if runner == nil {
		return nil, fmt.Errorf("intent runner required")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	if request.InvoiceID == uuid.Nil || request.PaymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("invoice_id and payment_method_id required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be between 1 and 255 bytes")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	key := intents.InvoiceCollectionRetryKey(request.InvoiceID, request.IdempotencyKey)
	origin, reason := intents.OriginAdmin, "manual invoice collection retry"
	if initiator == charge.InitiatorCustomer {
		key = charge.CustomerPaymentKey(TypeInvoiceCollection, payer.UUID(), request.IdempotencyKey)
		origin, reason = intents.OriginUser, "verified customer invoice payment"
	}
	pm := request.PaymentMethodID
	intentID, replayed, err := s.enqueueInvoiceCollection(ctx, payer, request.InvoiceID, invoiceCollectionEnqueue{
		manual: true, paymentMethodID: &pm, operationKey: key, origin: origin, originReason: reason, initiator: initiator,
	})
	if err != nil {
		return nil, err
	}
	if intentID == uuid.Nil {
		return nil, ErrInvoiceNotRetryable
	}
	operation, err := runner.ExecuteByID(ctx, intentID)
	if err != nil {
		return nil, fmt.Errorf("retry invoice collection: %w", err)
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	invoice, err := s.GetInvoiceByID(loadCtx, payer, request.InvoiceID)
	if err != nil {
		return nil, err
	}
	out := &InvoiceCollectionRetryResult{Invoice: invoice, Replayed: replayed, Operation: operation}
	if operation.Status != intents.StatusSucceeded {
		return out, nil
	}
	p, err := intents.DecodeInvoiceCollectionPayload(operation)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(loadCtx).GetInvoicePayment(loadCtx, gen.GetInvoicePaymentParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: request.InvoiceID, ID: p.PaymentID})
	if err != nil {
		return nil, fmt.Errorf("load invoice payment: %w", err)
	}
	if out.Payment, err = models.PaymentFromGen(row); err != nil {
		return nil, err
	}
	return out, nil
}

func scheduledInvoiceCollectionKey(invoiceID uuid.UUID, ordinal int64) string {
	return fmt.Sprintf("%s:%s:attempt:%d", TypeInvoiceCollection, invoiceID, ordinal)
}

// invoiceCollectionRetryable gates the MANUAL retry surface: an uncollectible
// invoice, or an open one that has failed at least once (or#828) or is past
// its due date. A never-attempted invoice not yet due is not "retryable".
func invoiceCollectionRetryable(invoice *models.Invoice, now time.Time) bool {
	return invoice != nil && invoice.CollectionIntentID == nil &&
		invoice.CollectionMethod == CollectionChargeAutomatically &&
		(invoice.Status == "uncollectible" ||
			(invoice.Status == "open" && (invoice.CollectionFailureCount > 0 || (invoice.DueAt != nil && invoice.DueAt.Before(now))))) &&
		invoice.AmountDue > 0
}

func scheduledInvoiceCollectionEligible(invoice *models.Invoice, minThreshold int64, now time.Time) bool {
	if invoice == nil || invoice.CollectionIntentID != nil || invoice.CollectionMethod != CollectionChargeAutomatically ||
		invoice.Status != "open" || invoice.AmountDue <= 0 ||
		(minThreshold > 0 && invoice.AmountDue < minThreshold) {
		return false
	}
	if invoice.DueAt != nil && invoice.DueAt.After(now) {
		return false
	}
	return invoice.CollectionFailureCount == 0 && invoice.NextCollectionAttemptAt == nil ||
		invoice.NextCollectionAttemptAt != nil && !invoice.NextCollectionAttemptAt.After(now)
}

type invoiceCollectionEnqueue struct {
	initiator       charge.Initiator
	minThreshold    int64
	manual          bool
	paymentMethodID *uuid.UUID
	operationKey    string
	origin          intents.Origin
	originReason    string
}

// enqueueInvoiceCollection freezes one collection attempt atomically: the
// intent and the invoice's pointer at the operation commit together, or
// nothing does. A client retry key is looked up under
// the invoice lock, so two equal concurrent retries serialize on the row and
// the second replays the first's operation instead of seeing its in-flight
// state (replayed=true). intentID == Nil means the invoice was not eligible
// (scheduled path) — the manual path reports why.
func (s *MoneyService) enqueueInvoiceCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, opts invoiceCollectionEnqueue) (intentID uuid.UUID, replayed bool, err error) {
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
			return fmt.Errorf("lock invoice payer: %w", err)
		}
		row, err := q.GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: invoiceID})
		if errors.Is(err, pgx.ErrNoRows) && !opts.manual {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock invoice: %w", err)
		}
		invoice, err := invoiceFromGen(row)
		if err != nil {
			return err
		}
		if opts.operationKey != "" {
			prior, err := q.GetProviderIntentByIdempotencyKey(ctx, gen.GetProviderIntentByIdempotencyKeyParams{MerchantID: tid.UUID(), IdempotencyKey: opts.operationKey})
			switch {
			case err == nil:
				frozen, err := intents.DecodeInvoiceCollectionPayload(prior)
				if err != nil {
					return err
				}
				if frozen.InvoiceID != invoiceID || frozen.CustomerID != payer.UUID() || opts.paymentMethodID == nil || frozen.PaymentMethodID != *opts.paymentMethodID || frozen.Initiator != opts.initiator {
					return ErrInvoiceRetryIdempotencyConflict
				}
				intentID, replayed = prior.ID, true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("load retry operation: %w", err)
			}
		}
		if invoice.CollectionIntentID != nil {
			if !opts.manual {
				return nil
			}
			live, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: tid.UUID(), ID: *invoice.CollectionIntentID})
			if err == nil && live.Status == intents.StatusUnknownNeedsVerify {
				return ErrInvoiceRetryOutcomeUnknown
			}
			return ErrInvoiceRetryInProgress
		}
		eligible := scheduledInvoiceCollectionEligible(invoice, opts.minThreshold, now)
		if opts.manual {
			eligible = invoiceCollectionRetryable(invoice, now)
			if opts.initiator == charge.InitiatorCustomer {
				eligible = invoice.CollectionIntentID == nil && invoice.AmountDue > 0 && (invoice.Status == "open" || invoice.Status == "uncollectible")
			}
		}
		if !eligible {
			return nil
		}
		method, err := s.collectionMethodFor(ctx, q, tid.UUID(), payer.UUID(), invoice, opts)
		if errors.Is(err, charge.ErrAgreementRequired) && !opts.manual {
			return stopCollectionForAgreement(ctx, s.db.NewWithPgxTx(tx), invoice, now)
		}
		if err != nil || method == nil {
			return err
		}
		// The account the charge goes through: the card's own PSP, or the one
		// PSP of its rail that reaches its custodian.
		psp, err := charge.RoutePSP(ctx, q, *method)
		if err != nil {
			return fmt.Errorf("route invoice %s collection: %w", invoice.ID, err)
		}
		receipt, found, err := s.findObservedInvoicePayment(ctx, q, tid.UUID(), invoice.ID, psp)
		if err != nil {
			return err
		}
		if found {
			local := NewMoneyService(s.db.NewWithPgxTx(tx), s.Clock())
			_, err := local.RecoverObservedInvoicePayment(ctx, receipt)
			return err // committed readback; no new operation or invented initiator
		}
		// A recent read on the newly selected account cannot exclude a payment
		// through another known card account after the restored snapshot.
		accounts, err := q.InvoiceRecoveryAccounts(ctx, gen.InvoiceRecoveryAccountsParams{MerchantID: tid.UUID(), RoutedPsp: psp})
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
			binding, err := collectionHyperSwitchBinding(ctx, q, *method, psp, s.hyperSwitchDeployment)
			if err != nil {
				return err
			}
			custody = &binding
		}
		instrument, err := collectionInstrument(ctx, q, *method, psp, invoice.Currency, opts.initiator)
		if err != nil {
			return err
		}
		amountMinor, err := moneyutil.NativeToRailMinor(invoice.Currency, invoice.AmountDue)
		if err != nil {
			return fmt.Errorf("invoice %s amount is not representable on rail %s: %w", invoice.ID, method.Rail, err)
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
		if _, err := moneyutil.RailMinorToNative(invoice.Currency, amountMinor); err != nil {
			return fmt.Errorf("invoice %s rounded charge is not representable: %w", invoice.ID, err)
		}
		key := opts.operationKey
		if key == "" {
			key = scheduledInvoiceCollectionKey(invoiceID, int64(invoice.CollectionAttemptCount))
		}
		paymentID := uuidutil.NewV7()
		intent, err := intents.NewStore(s.db.NewWithPgxTx(tx)).Enqueue(ctx, intents.EnqueueParams{
			MerchantID: tid.UUID(), Provider: normalizeRail(method.Rail), PspID: psp, IntentType: TypeInvoiceCollection,
			Payload: intents.InvoiceCollectionPayload{
				InvoiceID: invoiceID, CustomerID: payer.UUID(), PaymentID: paymentID, PaymentMethodID: method.ID, Initiator: opts.initiator,
				Rail: normalizeRail(method.Rail), Instrument: instrument,
				HyperSwitch: custody,
				Currency:    invoice.Currency, Amount: invoice.AmountDue, AmountMinor: amountMinor,
				ProviderCustomerRef: providerCustomerRef,
				Description:         fmt.Sprintf("invoice %s", invoiceID),
			},
			IdempotencyKey: key, NextAttemptAt: now, Origin: opts.origin, OriginReason: opts.originReason,
			Actor: func() string {
				if opts.initiator == charge.InitiatorCustomer {
					return payer.UUID().String()
				}
				return ""
			}(),
		})
		if err != nil {
			return fmt.Errorf("enqueue invoice collection: %w", err)
		}
		canonical, err := intents.DecodeInvoiceCollectionPayload(intent)
		if err != nil {
			return err
		}
		if canonical.InvoiceID != invoiceID || canonical.CustomerID != payer.UUID() || canonical.PaymentMethodID != method.ID || canonical.Initiator != opts.initiator {
			return ErrInvoiceRetryIdempotencyConflict
		}
		if canonical.PaymentID != paymentID {
			intentID, replayed = intent.ID, true
			return nil
		}
		claimed, err := q.ClaimInvoiceCollection(ctx, gen.ClaimInvoiceCollectionParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID, IntentID: intent.ID, Now: now})
		if err != nil {
			return fmt.Errorf("claim invoice collection: %w", err)
		}
		if claimed != 1 {
			return errors.New("claim invoice collection: invoice changed under lock")
		}
		intentID = intent.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return intentID, replayed, nil
}

// collectionMethodFor resolves the payer-owned saved method the attempt is
// charged through: the explicitly bound one (manual retry) or the account's
// collection method. or#893: the account that vaulted the instrument takes
// the money. The row is read under a shared lock so the instrument the
// operation freezes cannot be remapped before the operation commits.
func (s *MoneyService) collectionMethodFor(ctx context.Context, q *gen.Queries, merchantID, payerID uuid.UUID, invoice *models.Invoice, opts invoiceCollectionEnqueue) (*gen.BillingPaymentMethod, error) {
	id := opts.paymentMethodID
	if id == nil {
		settingsRow, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: merchantID, CustomerID: payerID, Currency: invoice.Currency})
		if errors.Is(err, pgx.ErrNoRows) {
			if opts.manual {
				return nil, ErrCollectionPaymentMethodRequired
			}
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load invoice collection settings: %w", err)
		}
		id = collectionPaymentMethodID(settingsFromGen(settingsRow))
		if id == nil {
			if opts.manual {
				return nil, ErrCollectionPaymentMethodRequired
			}
			return nil, nil
		}
	}
	method, err := q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: *id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCollectionPaymentMethodInvalid
		}
		return nil, fmt.Errorf("load collection payment method: %w", err)
	}
	if method.MerchantID != merchantID || method.CustomerID != payerID || strings.TrimSpace(models.DerefStr(method.ParkReason)) != "" {
		return nil, ErrCollectionPaymentMethodInvalid
	}
	if descriptor, ok := rails.Lookup(models.Rail(method.Rail)); !ok || !descriptor.SupportsChargeSavedMethod {
		return nil, ErrCollectionPaymentMethodInvalid
	}
	if opts.initiator == charge.InitiatorCustomer && (!rails.IsNMI(models.Rail(method.Rail)) || (method.Custodian != models.CustodianPSP && method.Custodian != models.CustodianHyperSwitch)) {
		return nil, ErrCustomerPaymentUnsupported
	}
	if opts.initiator == charge.InitiatorMerchant {
		if err := requireCollectionAgreement(ctx, q, method, invoice.Currency); err != nil {
			if opts.manual && errors.Is(err, charge.ErrAgreementRequired) {
				return nil, fmt.Errorf("%w: %w", ErrCollectionPaymentMethodInvalid, err)
			}
			return nil, err
		}
	}
	return &method, nil
}

// requireCollectionAgreement refuses a merchant-initiated collection in
// currency on a card without the customer's active unscheduled mandate for it
// on the account that charges it (#1166): never given, or waiting for consent
// after the card was reissued under another brand.
func requireCollectionAgreement(ctx context.Context, q *gen.Queries, method gen.BillingPaymentMethod, currency string) error {
	psp, err := charge.RoutePSP(ctx, q, method)
	if err != nil {
		return err
	}
	_, err = mandates.ForCollection(ctx, q, method.MerchantID, method.CustomerID, currency, method.ID, psp)
	return err
}

// stopCollectionForAgreement stops collecting an invoice whose collection
// card carries no agreement for a merchant-initiated charge, and asks the
// customer to act. Designating a card resumes it.
func stopCollectionForAgreement(ctx context.Context, d *db.DB, invoice *models.Invoice, now time.Time) error {
	stopped, err := d.Gen(ctx).StopInvoiceCollection(ctx, gen.StopInvoiceCollectionParams{
		MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
		FailureCode: charge.AgreementRequiredCode, FailureMessage: charge.ErrAgreementRequired.Error(), Now: now,
	})
	if err != nil {
		return fmt.Errorf("stop invoice collection: %w", err)
	}
	if stopped != 1 {
		return errors.New("stop invoice collection: invoice changed under lock")
	}
	action := collection.Action{Decline: decline.Result{Action: decline.FixPaymentMethod}}
	return queueInvoiceCollectionOutcome(ctx, d, invoice, action, charge.AgreementRequiredCode, now)
}

// collectionInstrument freezes an invoice charge's card and the mandate it
// cites: a merchant-initiated collection runs under the currency's active
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
