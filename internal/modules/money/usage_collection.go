package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/merchants"
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
	"github.com/open-rails/openrails/internal/modules/invoices/usage"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

var (
	ErrCustomerPaymentUnsupported    = errors.New("customer-present payment is not supported for this rail")
	ErrUsageNotCollectible           = errors.New("the usage invoice takes no collection now")
	ErrDefaultPaymentMethodRequired  = errors.New("no default payment method for the currency")
	ErrDefaultPaymentMethodInvalid   = errors.New("the default payment method cannot pay this invoice")
	ErrCollectionInProgress          = errors.New("a collection of the invoice is in progress")
	ErrCollectionOutcomeUnknown      = errors.New("the invoice's collection outcome is unknown; resolve the operation before another attempt")
	ErrCollectionIdempotencyConflict = errors.New("the key names another collection")
)

// UsageCollectionRequest charges a usage invoice to one of its customer's
// saved cards; one IdempotencyKey is one collection.
type UsageCollectionRequest struct {
	InvoiceID       uuid.UUID
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

// CollectDueUsageInvoice queues the scheduled collection of a usage invoice whose
// next charge is due: one order_collection operation, which its own job
// charges. An invoice whose customer has no card that can pay it waits until a
// card is designated; one of a merchant whose rebills are held waits for
// their release.
func (s *MoneyService) CollectDueUsageInvoice(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("money service not initialized")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if c, err := (controls.View{DB: s.db}).Get(ctx, mid.UUID()); err != nil || c.RebillsHeld {
		return err
	}
	_, _, err = s.enqueueUsageCollection(ctx, payer, invoiceID, usageCollectionEnqueue{origin: intents.OriginSystem, originReason: "scheduled usage collection"})
	return err
}

// PayUsageNow is the customer paying their usage invoice with a saved card,
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
	if request.InvoiceID == uuid.Nil || request.PaymentMethodID == uuid.Nil {
		return nil, fmt.Errorf("invoice and payment method required")
	}
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" || len(request.IdempotencyKey) > 255 {
		return nil, fmt.Errorf("idempotency_key must be between 1 and 255 bytes")
	}
	key := charge.CustomerPaymentKey(TypeInvoiceCollection, payer.UUID(), request.IdempotencyKey)
	pm := request.PaymentMethodID
	intentID, replayed, err := s.enqueueUsageCollection(ctx, payer, request.InvoiceID, usageCollectionEnqueue{
		manual: true, paymentMethodID: &pm, operationKey: key, origin: intents.OriginUser, originReason: "verified customer usage payment", initiator: charge.InitiatorCustomer,
	})
	if err != nil {
		return nil, err
	}
	if intentID == uuid.Nil {
		return nil, ErrUsageNotCollectible
	}
	operation, err := runner.ExecuteFirstByID(ctx, intentID)
	if err != nil {
		return nil, fmt.Errorf("collect usage invoice: %w", err)
	}
	return &UsageCollection{Replayed: replayed, Operation: operation}, nil
}

func scheduledUsageCollectionKey(invoiceID uuid.UUID, ordinal int64) string {
	return fmt.Sprintf("%s:%s:attempt:%d", TypeInvoiceCollection, invoiceID, ordinal)
}

func scheduledUsageCollectionEligible(invoice gen.BillingInvoice, now time.Time) bool {
	if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) || invoice.Status != string(billing.InvoiceOpen) || invoice.CollectionMethod == nil || *invoice.CollectionMethod != CollectionChargeAutomatically ||
		usage.Due(invoice) <= 0 {
		return false
	}
	return invoice.NextCollectionAttemptAt != nil && !invoice.NextCollectionAttemptAt.After(now)
}

type usageCollectionEnqueue struct {
	initiator charge.Initiator
	// read resolves the method without locks: whether a collection could
	// charge, as a read-only snapshot asks it.
	read            bool
	manual          bool
	paymentMethodID *uuid.UUID
	operationKey    string
	origin          intents.Origin
	originReason    string
}

// enqueueUsageCollection freezes one collection atomically: the intent and
// the invoice's live attempt commit together, or nothing does. A client key is
// looked up under the invoice lock, so two equal concurrent requests serialize
// on the row and the second replays the first's operation (replayed=true).
// intentID == Nil means the invoice was not eligible (scheduled path); the
// manual path reports why.
func (s *MoneyService) enqueueUsageCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, opts usageCollectionEnqueue) (intentID uuid.UUID, replayed bool, err error) {
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
		invoice, err := q.LockUsageInvoice(ctx, gen.LockUsageInvoiceParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), ID: invoiceID})
		if errors.Is(err, pgx.ErrNoRows) && !opts.manual {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock usage invoice: %w", err)
		}
		if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
			if opts.manual {
				return ErrUsageActionNotAllowed
			}
			return nil
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
					return ErrCollectionIdempotencyConflict
				}
				intentID, replayed = prior.ID, true
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("load collection operation: %w", err)
			}
		}
		if invoice.Status == string(billing.InvoiceProcessing) {
			if !opts.manual {
				return nil
			}
			if invoice.AttemptID != nil {
				live, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: tid.UUID(), ID: *invoice.AttemptID})
				if err == nil && live.Status == intents.StatusUnknownNeedsVerify {
					return ErrCollectionOutcomeUnknown
				}
			}
			return ErrCollectionInProgress
		}
		eligible := scheduledUsageCollectionEligible(invoice, now)
		if opts.manual {
			eligible = opts.initiator == charge.InitiatorCustomer && usage.Due(invoice) > 0 &&
				(invoice.Status == string(billing.InvoiceOpen) || invoice.Status == string(billing.InvoiceUncollectible))
		}
		if !eligible {
			return nil
		}
		method, err := s.collectionMethodFor(ctx, q, tid.UUID(), payer.UUID(), invoice.Currency, opts)
		if errors.Is(err, charge.ErrAgreementRequired) && !opts.manual {
			return stopCollectionForAgreement(ctx, s.db.NewWithPgxTx(tx), invoice, now)
		}
		if !opts.manual && (err == nil && method == nil || errors.Is(err, ErrDefaultPaymentMethodInvalid)) {
			// No card can pay it: charging waits until one is designated.
			_, err := q.ParkUsageCollection(ctx, gen.ParkUsageCollectionParams{MerchantID: tid.UUID(), ID: invoice.ID, Now: now})
			return err
		}
		if err != nil || method == nil {
			return err
		}
		// The account the charge goes through: the card's own PSP, or the one
		// PSP of its rail that reaches its custodian.
		psp, err := charge.RoutePSP(ctx, charge.CustodyOf(s.db), *method)
		if err != nil {
			return fmt.Errorf("route invoice %s collection: %w", invoice.ID, err)
		}
		receipt, found, err := s.findObservedUsagePayment(ctx, q, tid.UUID(), invoice.ID, psp)
		if err != nil {
			return err
		}
		if found {
			local := NewMoneyService(s.db.NewWithPgxTx(tx), s.Clock())
			_, err := local.RecoverObservedUsagePayment(ctx, receipt)
			return err // committed readback; no new operation or invented initiator
		}
		var custody *charge.HyperSwitchBinding
		if method.Custodian == models.CustodianHyperSwitch {
			binding, err := charge.FreezeHyperSwitchBinding(ctx, q, charge.CustodyOf(s.db), *method, psp, s.hyperSwitchDeployment)
			if err != nil {
				return err
			}
			custody = &binding
		}
		instrument, err := collectionInstrument(ctx, q, *method, psp, invoice.Currency, opts.initiator)
		if err != nil {
			return err
		}
		due := usage.Due(invoice)
		amountMinor, err := moneyutil.NativeToRailMinor(invoice.Currency, due)
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
			ordinal, err := q.CountInvoiceAttempts(ctx, gen.CountInvoiceAttemptsParams{MerchantID: tid.UUID(), InvoiceID: invoiceID})
			if err != nil {
				return err
			}
			key = scheduledUsageCollectionKey(invoiceID, ordinal)
		}
		paymentID := uuidutil.NewV7()
		intent, err := intents.NewStore(s.db.NewWithPgxTx(tx)).Enqueue(ctx, intents.EnqueueParams{
			MerchantID: tid.UUID(), Provider: normalizeRail(method.Rail), PspID: psp, IntentType: TypeInvoiceCollection,
			Payload: intents.InvoiceCollectionPayload{
				InvoiceCaptchaSubjects: intents.InvoiceCaptchaSubjects(ctx),
				InvoiceID:              invoiceID, CustomerID: payer.UUID(), PaymentID: paymentID, PaymentMethodID: method.ID, Initiator: opts.initiator,
				Rail: normalizeRail(method.Rail), Instrument: instrument,
				HyperSwitch: custody,
				Currency:    invoice.Currency, Amount: due, AmountMinor: amountMinor,
				ProviderCustomerRef: providerCustomerRef,
				// The provider's description names the invoice as the invoice it
				// renders; recovery finds a collection's sale by it.
				Description: fmt.Sprintf("invoice %s", invoiceID),
			},
			// A customer's own payment runs in its request; a scheduled one in
			// its operation's job.
			IdempotencyKey: key, NextAttemptAt: now, Origin: opts.origin, OriginReason: opts.originReason, Inline: opts.manual,
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
			return ErrCollectionIdempotencyConflict
		}
		if canonical.PaymentID != paymentID {
			intentID, replayed = intent.ID, true
			return nil
		}
		if err := usage.StartCollection(ctx, s.db.NewWithPgxTx(tx), invoice, intent.ID, psp, normalizeRail(method.Rail), method.ID, due, now); err != nil {
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
// instrument the operation freezes cannot be remapped before it commits;
// a read (opts.read) takes none.
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
	var method gen.BillingPaymentMethod
	var err error
	if opts.read {
		method, err = q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: *id})
	} else {
		method, err = q.GetPaymentMethodForShare(ctx, gen.GetPaymentMethodForShareParams{MerchantID: merchantID, ID: *id})
	}
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
		if err := requireCollectionAgreement(ctx, q, charge.CustodyOf(s.db), method, currency, opts.read); err != nil {
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
func requireCollectionAgreement(ctx context.Context, q *gen.Queries, custody *merchants.Service, method gen.BillingPaymentMethod, currency string, read bool) error {
	psp, err := charge.RoutePSP(ctx, custody, method)
	if err != nil {
		return err
	}
	if read {
		_, err = mandates.ReadForCollection(ctx, q, method.MerchantID, method.CustomerID, currency, method.ID, psp)
	} else {
		_, err = mandates.ForCollection(ctx, q, method.MerchantID, method.CustomerID, currency, method.ID, psp)
	}
	return err
}

// stopCollectionForAgreement stops collecting a usage invoice whose collection
// card carries no agreement for a merchant-initiated charge, and asks the
// customer to act. Designating a card resumes it.
func stopCollectionForAgreement(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) error {
	failure := billing.PaymentFailure{Reason: charge.AgreementRequiredCode, Message: charge.ErrAgreementRequired.Error()}
	if err := usage.StopCollection(ctx, d.Gen(ctx), invoice.MerchantID, invoice.ID, failure, now); err != nil {
		return err
	}
	action := collection.Action{Decline: decline.Result{Action: decline.FixPaymentMethod}}
	// No attempt or decline names this stop: once stopped, the invoice is not
	// stopped again until a card resumes it.
	return queueCollectionOutcome(ctx, d, uuidutil.NewV7(), invoice, action, charge.AgreementRequiredCode, now)
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
