package money

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

var (
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

type InvoiceCollectionRetryResult struct {
	Invoice  *models.Invoice
	Attempt  models.InvoicePaymentAttempt
	Replayed bool
}

// ListInvoicePaymentAttempts returns one payer-owned invoice's collection
// history, newest attempt first.
func (s *MoneyService) ListInvoicePaymentAttempts(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, limit, offset int) ([]models.InvoicePaymentAttempt, int, error) {
	if s == nil || s.db == nil {
		return nil, 0, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, 0, fmt.Errorf("payer required")
	}
	if invoiceID == uuid.Nil {
		return nil, 0, fmt.Errorf("invoice_id required")
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Distinguish an existing invoice with no attempts from an inaccessible ID.
	if _, err := s.GetInvoiceByID(ctx, payer, invoiceID); err != nil {
		return nil, 0, fmt.Errorf("load invoice: %w", err)
	}

	q := s.db.Gen(ctx)
	total, err := q.CountInvoicePaymentAttemptsByPayer(ctx, gen.CountInvoicePaymentAttemptsByPayerParams{
		MerchantID: tid.UUID(),
		CustomerID: payer.UUID(),
		InvoiceID:  invoiceID,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("count invoice payment attempts: %w", err)
	}
	rows, err := q.ListInvoicePaymentAttemptsByPayer(ctx, gen.ListInvoicePaymentAttemptsByPayerParams{
		MerchantID: tid.UUID(),
		CustomerID: payer.UUID(),
		InvoiceID:  invoiceID,
		Limit:      int64(limit),
		Offset:     int64(offset),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("list invoice payment attempts: %w", err)
	}
	attempts := make([]models.InvoicePaymentAttempt, 0, len(rows))
	for _, row := range rows {
		attempts = append(attempts, invoicePaymentAttemptFromGen(row))
	}
	return attempts, int(total), nil
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
		method, err := q.GetPaymentMethodByID(ctx, paymentMethodID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCollectionPaymentMethodInvalid
			}
			return fmt.Errorf("load collection payment method: %w", err)
		}
		if method.MerchantID != tid.UUID() || method.CustomerID != payer.UUID() {
			return ErrCollectionPaymentMethodInvalid
		}
		if strings.TrimSpace(method.ParkReason) != "" {
			return fmt.Errorf("%w: payment method is parked", ErrCollectionPaymentMethodInvalid)
		}
		descriptor, ok := rails.Lookup(models.Rail(method.Rail))
		if !ok {
			return fmt.Errorf("%w: unknown rail %q", ErrCollectionPaymentMethodInvalid, method.Rail)
		}
		if !descriptor.SupportsChargeSavedMethod {
			return fmt.Errorf("%w: rail %q does not support invoice collection", ErrCollectionPaymentMethodInvalid, method.Rail)
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

// RetryInvoiceCollection binds a client retry key to one saved payment method
// and runs the durable collection operation. Reusing the key returns that
// operation's durable state without another provider charge; a different
// method under the same key is a conflict.
func (s *MoneyService) RetryInvoiceCollection(ctx context.Context, runner *intents.Runner, payer identity.CustomerID, request InvoiceCollectionRetryRequest) (*InvoiceCollectionRetryResult, error) {
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
	key := invoiceRetryOperationKey(request.InvoiceID, request.IdempotencyKey)
	pm := request.PaymentMethodID
	intentID, replayed, err := s.enqueueInvoiceCollection(ctx, payer, request.InvoiceID, invoiceCollectionEnqueue{
		manual: true, paymentMethodID: &pm, operationKey: key, origin: intents.OriginAdmin, originReason: "manual invoice collection retry",
	})
	if err != nil {
		return nil, err
	}
	if intentID == uuid.Nil {
		return nil, ErrInvoiceNotRetryable
	}
	if _, err := runner.ExecuteByID(ctx, intentID); err != nil {
		return nil, fmt.Errorf("retry invoice collection: %w", err)
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	invoice, err := s.GetInvoiceByID(loadCtx, payer, request.InvoiceID)
	if err != nil {
		return nil, err
	}
	attemptRow, err := s.db.Gen(loadCtx).GetInvoicePaymentAttemptByKey(loadCtx, gen.GetInvoicePaymentAttemptByKeyParams{
		MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: request.InvoiceID, IdempotencyKey: &key,
	})
	if err != nil {
		return nil, fmt.Errorf("load invoice retry outcome: %w", err)
	}
	return &InvoiceCollectionRetryResult{Invoice: invoice, Attempt: invoicePaymentAttemptFromGen(attemptRow), Replayed: replayed}, nil
}

// invoiceRetryOperationKey binds one client retry key to one invoice.
func invoiceRetryOperationKey(invoiceID uuid.UUID, clientKey string) string {
	digest := sha256.Sum256([]byte(invoiceID.String() + "\x00" + clientKey))
	return fmt.Sprintf("%s:%s:retry:%s", TypeInvoiceCollection, invoiceID, hex.EncodeToString(digest[:16]))
}

func scheduledInvoiceCollectionKey(invoiceID uuid.UUID, ordinal int64) string {
	return fmt.Sprintf("%s:%s:attempt:%d", TypeInvoiceCollection, invoiceID, ordinal)
}

// invoiceCollectionRetryable gates the MANUAL retry surface. `open` counts once
// the invoice has failed at least once (or#828): a bucket-2 stop deliberately
// leaves the status alone. A never-attempted open invoice is not "retryable".
func invoiceCollectionRetryable(invoice *models.Invoice) bool {
	return invoice != nil && invoice.CollectionIntentID == nil &&
		invoice.CollectionMethod == CollectionChargeAutomatically &&
		(invoice.Status == "past_due" || invoice.Status == "uncollectible" ||
			(invoice.Status == "open" && invoice.CollectionFailureCount > 0)) &&
		invoice.AmountDue > 0
}

func scheduledInvoiceCollectionEligible(invoice *models.Invoice, minThreshold int64, now time.Time) bool {
	if invoice == nil || invoice.CollectionIntentID != nil || invoice.CollectionMethod != CollectionChargeAutomatically ||
		(invoice.Status != "open" && invoice.Status != "past_due") || invoice.AmountDue <= 0 ||
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
	minThreshold    int64
	manual          bool
	paymentMethodID *uuid.UUID
	operationKey    string
	origin          intents.Origin
	originReason    string
}

// enqueueInvoiceCollection freezes one collection attempt atomically: the
// intent, its invoice_payments row and the invoice's pointer at the operation
// commit together, or nothing does. A client retry key is looked up under
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
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
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
			prior, err := q.GetRailIntentByIdempotencyKey(ctx, gen.GetRailIntentByIdempotencyKeyParams{MerchantID: tid.UUID(), IdempotencyKey: opts.operationKey})
			switch {
			case err == nil:
				frozen, err := decodeInvoiceCollectionPayload(prior)
				if err != nil {
					return err
				}
				if frozen.InvoiceID != invoiceID || frozen.CustomerID != payer.UUID() || opts.paymentMethodID == nil || frozen.PaymentMethodID != *opts.paymentMethodID {
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
			live, err := q.GetRailIntent(ctx, *invoice.CollectionIntentID)
			if err == nil && live.Status == intents.StatusUnknownNeedsVerify {
				return ErrInvoiceRetryOutcomeUnknown
			}
			return ErrInvoiceRetryInProgress
		}
		eligible := scheduledInvoiceCollectionEligible(invoice, opts.minThreshold, now)
		if opts.manual {
			eligible = invoiceCollectionRetryable(invoice)
		}
		if !eligible {
			return nil
		}
		method, err := s.collectionMethodFor(ctx, q, tid.UUID(), payer.UUID(), invoice, opts)
		if err != nil || method == nil {
			return err
		}
		amountMinor, err := moneyutil.NativeToRailMinor(invoice.Currency, invoice.AmountDue)
		if err != nil {
			return fmt.Errorf("invoice %s amount is not representable on rail %s: %w", invoice.ID, method.Rail, err)
		}
		attempts, err := q.CountInvoicePaymentAttemptsByPayer(ctx, gen.CountInvoicePaymentAttemptsByPayerParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID})
		if err != nil {
			return fmt.Errorf("count invoice collection attempts: %w", err)
		}
		key := opts.operationKey
		if key == "" {
			key = scheduledInvoiceCollectionKey(invoiceID, attempts)
		}
		attemptID := uuidutil.NewV7()
		intent, err := intents.NewStore(s.db.NewWithPgxTx(tx)).Enqueue(ctx, intents.EnqueueParams{
			MerchantID: tid.UUID(), Provider: normalizeRail(method.Rail), PspID: method.PspID, IntentType: TypeInvoiceCollection,
			Payload: InvoiceCollectionPayload{
				InvoiceID: invoiceID, CustomerID: payer.UUID(), AttemptID: attemptID, PaymentMethodID: method.ID,
				Rail: normalizeRail(method.Rail), Instrument: CollectionInstrumentOf(*method),
				Currency: invoice.Currency, Amount: invoice.AmountDue, AmountMinor: amountMinor,
				Description: fmt.Sprintf("invoice %s", invoiceID),
			},
			IdempotencyKey: key, NextAttemptAt: now, Origin: opts.origin, OriginReason: opts.originReason,
		})
		if err != nil {
			return fmt.Errorf("enqueue invoice collection: %w", err)
		}
		if err := q.InsertInvoicePayment(ctx, gen.InsertInvoicePaymentParams{
			ID: attemptID, MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceID: invoiceID,
			Currency: invoice.Currency, Amount: invoice.AmountDue, Status: "attempted",
			AttemptedAt: now, CreatedAt: now, UpdatedAt: now,
			PaymentMethodID: &method.ID, IdempotencyKey: &key, PspID: &method.PspID,
		}); err != nil {
			return fmt.Errorf("record invoice collection attempt: %w", err)
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
func (s *MoneyService) collectionMethodFor(ctx context.Context, q *gen.Queries, merchantID, payerID uuid.UUID, invoice *models.Invoice, opts invoiceCollectionEnqueue) (*gen.OpenrailsPaymentMethod, error) {
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
	if method.MerchantID != merchantID || method.CustomerID != payerID || strings.TrimSpace(method.ParkReason) != "" {
		return nil, ErrCollectionPaymentMethodInvalid
	}
	if descriptor, ok := rails.Lookup(models.Rail(method.Rail)); !ok || !descriptor.SupportsChargeSavedMethod {
		return nil, ErrCollectionPaymentMethodInvalid
	}
	return &method, nil
}
