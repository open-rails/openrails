package riverjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// TypeAccountUpdater is the account-updater request for one stored card. It
// is billed and may mint a token at the vault, so it is a provider write: the
// intent's id is the vault's idempotency key, and a repeat inside the vault's
// 24-hour idempotency window returns the first answer.
const TypeAccountUpdater = "account_updater"

// accountUpdaterWindow is the vault's idempotency window; past it a retry
// would be a second billed request, so the intent expires instead.
const accountUpdaterWindow = 24 * time.Hour

// StoredCardRefreshWorker refreshes one stored card from a holder that must be
// asked. A decline queues it with Declined; the ahead-of-renewal pass without.
type StoredCardRefreshWorker struct {
	river.WorkerDefaults[paymentmethods.StoredCardRefreshArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	Updaters  paymentmethods.AccountUpdaters
	Intents   *intents.Runner
	Lifecycle *subscriptions.SubscriptionLifecycleService
}

func (StoredCardRefreshWorker) Kind() string { return paymentmethods.KindStoredCardRefresh }

func (w *StoredCardRefreshWorker) Work(ctx context.Context, job *river.Job[paymentmethods.StoredCardRefreshArgs]) error {
	if w.DB == nil || w.Updaters == nil || w.Intents == nil || w.Lifecycle == nil {
		return errors.New("stored card refresh: not wired")
	}
	args := job.Args
	return w.DB.RunInMerchantScope(ctx, billing.MerchantID(args.MerchantID), "stored card refresh", func(ctx context.Context) error {
		method, err := w.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: args.MerchantID, ID: args.PaymentMethodID})
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if method.Status != paymentmethods.StatusActive {
			return nil
		}
		now := workerNow(w.Clock)
		ref := models.DerefStr(method.RailMethodRef)
		if reissued, checked := args.Moot(method); reissued || checked {
			if !args.Declined {
				return nil
			}
			// Overtaken since the decline: a reissued card retries its
			// renewals; one checked without a new card asks the customer.
			if !reissued {
				return w.askUnlessReissued(ctx, args.MerchantID, method.ID, ref, now)
			}
			return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				return subscriptions.WakeForReplacedMethod(ctx, w.DB.NewWithPgxTx(tx), args.MerchantID, method.ID, now)
			})
		}
		logger := log.WithContext(ctx).WithFields(log.Fields{"payment_method_id": method.ID, "declined": args.Declined})
		_, armed, err := w.Updaters.AccountUpdater(ctx, method)
		if err != nil {
			if job.Attempt < paymentmethods.StoredCardRefreshAttempts {
				return fmt.Errorf("stored card refresh: resolve holder: %w", err)
			}
			logger.WithError(err).Warn("stored card refresh: holder unresolved")
		}
		if armed {
			if blocked, reason := intents.GateExecution(ctx, w.Intents.Config, args.MerchantID, intents.OriginSystem); blocked {
				logger.WithField("reason", reason).Info("stored card refresh: provider writes are held")
				armed = false
			}
		}
		if !armed {
			if !args.Declined {
				return nil
			}
			return w.askUnlessReissued(ctx, args.MerchantID, method.ID, ref, now)
		}
		checked := "never"
		if method.AccountUpdaterCheckedAt != nil {
			checked = strconv.FormatInt(method.AccountUpdaterCheckedAt.UnixNano(), 10)
		}
		cause := "renewal"
		if args.Declined {
			cause = "decline"
		}
		intent, err := w.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
			MerchantID:  args.MerchantID,
			Provider:    method.Custodian,
			CustodianID: *method.CustodianID,
			IntentType:  TypeAccountUpdater,
			Payload:     accountUpdaterPayload{PaymentMethodID: method.ID, RailMethodRef: ref},
			// One request per card, handle and last check: a retried job, a
			// re-queued card or a decline beside a renewal check share it.
			IdempotencyKey: TypeAccountUpdater + ":" + method.ID.String() + ":" + ref + ":" + checked,
			NextAttemptAt:  now,
			ExpiresAt:      ptrTime(now.Add(accountUpdaterWindow)),
			Origin:         intents.OriginSystem,
			OriginReason:   "stored card refresh: " + cause,
		})
		if err != nil || !args.Declined {
			return err
		}
		// A declined card's customer is asked unless the request reissued it.
		// A request still running is waited on, but never past the job's
		// attempts.
		if !intents.OperationTerminal(intent.Status) && job.Attempt < paymentmethods.StoredCardRefreshAttempts {
			return fmt.Errorf("stored card refresh: account update %s is %s", intent.ID, intent.Status)
		}
		return w.askUnlessReissued(ctx, args.MerchantID, method.ID, ref, now)
	})
}

// askUnlessReissued asks for a card whose handle did not change; a reissue
// already woke the renewals waiting on it, and a closed card asked already.
func (w *StoredCardRefreshWorker) askUnlessReissued(ctx context.Context, merchantID, methodID uuid.UUID, ref string, now time.Time) error {
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		current, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: merchantID, ID: methodID})
		if err != nil || current.Status != paymentmethods.StatusActive || models.DerefStr(current.RailMethodRef) != ref {
			return err
		}
		_, err = w.Lifecycle.AskForCard(ctx, d, merchantID, methodID, now)
		return err
	})
}

type accountUpdaterPayload struct {
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	RailMethodRef   string    `json:"rail_method_ref"`
}

// AccountUpdaterHandler runs one account-updater request and applies its
// answer through ApplyCardLifecycle. The decline that queued a request, not
// the request, asks the customer when no new card is found.
type AccountUpdaterHandler struct {
	DB        *db.DB
	Clock     clockwork.Clock
	Updaters  paymentmethods.AccountUpdaters
	Lifecycle *subscriptions.SubscriptionLifecycleService
	Policy    intents.BackoffPolicy
}

func (h *AccountUpdaterHandler) Type() string { return TypeAccountUpdater }

func (h *AccountUpdaterHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func decodeAccountUpdaterPayload(in gen.BillingProviderIntent) (accountUpdaterPayload, error) {
	var p accountUpdaterPayload
	if err := json.Unmarshal(in.Payload, &p); err != nil || p.PaymentMethodID == uuid.Nil || p.RailMethodRef == "" {
		return p, errors.New("account updater intent payload is incomplete")
	}
	return p, nil
}

// method is the card the intent was taken for, or ok=false when it no longer
// stands: closed, replaced, removed or given another handle.
func (h *AccountUpdaterHandler) method(ctx context.Context, p accountUpdaterPayload) (gen.BillingPaymentMethod, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingPaymentMethod{}, false, err
	}
	m, err := h.DB.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: p.PaymentMethodID})
	if db.IsNotFound(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	return m, m.Status == paymentmethods.StatusActive && models.DerefStr(m.RailMethodRef) == p.RailMethodRef, nil
}

func (h *AccountUpdaterHandler) CheckRelevance(ctx context.Context, in gen.BillingProviderIntent) (intents.Relevance, error) {
	p, err := decodeAccountUpdaterPayload(in)
	if err != nil {
		return intents.StillRelevant(), nil // Execute reports it
	}
	_, ok, err := h.method(ctx, p)
	if err != nil {
		return intents.Relevance{}, err
	}
	if !ok {
		return intents.SupersededBy("the card changed since the request was taken"), nil
	}
	return intents.StillRelevant(), nil
}

func (h *AccountUpdaterHandler) Execute(ctx context.Context, in gen.BillingProviderIntent) intents.Outcome {
	p, err := decodeAccountUpdaterPayload(in)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	m, ok, err := h.method(ctx, p)
	if err != nil {
		return intents.Retryable("load card: " + err.Error())
	}
	if !ok {
		return intents.Terminal("the card changed since the request was taken")
	}
	updater, armed, err := h.Updaters.AccountUpdater(ctx, m)
	if err != nil {
		return h.failed(ctx, in, m, "resolve holder: "+err.Error())
	}
	if !armed {
		return h.refused(ctx, m, "the card's holder is no longer armed for updates", false)
	}
	update, err := updater.Update(ctx, in.ID.String())
	switch {
	case errors.Is(err, basistheory.ErrProviderReadOnly):
		return intents.Parked("vault writes are blocked (mode=readonly)")
	case errors.Is(err, paymentmethods.ErrAccountUpdateRefused):
		return h.refused(ctx, m, err.Error(), true)
	case err != nil:
		return h.failed(ctx, in, m, err.Error())
	}
	change, err := h.apply(ctx, in, m, update)
	if err != nil {
		// The vault answers a repeat of this key with the same result.
		return intents.Retryable("apply account update: " + err.Error())
	}
	return intents.Succeeded(map[string]any{"result_code": update.ResultCode, "change": string(change)})
}

// apply records the answer as a card version and stamps the card checked, in
// one transaction.
func (h *AccountUpdaterHandler) apply(ctx context.Context, in gen.BillingProviderIntent, m gen.BillingPaymentMethod, update paymentmethods.AccountUpdate) (paymentmethods.CardChange, error) {
	now := workerNow(h.Clock)
	var life paymentmethods.CardLifecycle
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := h.DB.NewWithPgxTx(tx)
		if !update.Empty() {
			var err error
			life, _, err = h.Lifecycle.ApplyCardLifecycle(ctx, d, paymentmethods.CardEvent{MerchantID: m.MerchantID, PaymentMethodID: m.ID,
				Source: paymentmethods.SourceBasisTheoryUpdater, EventRef: TypeAccountUpdater + ":" + in.ID.String(), Card: update.Card, Advice: update.Advice, At: now})
			if err != nil {
				return err
			}
		}
		_, err := d.Gen(ctx).StampAccountUpdaterChecked(ctx, gen.StampAccountUpdaterCheckedParams{MerchantID: m.MerchantID, ID: m.ID, CheckedAt: now})
		return err
	})
	if err == nil && (life.Change != "" || update.ResultCode != basistheory.AUNoUpdate) {
		log.WithContext(ctx).WithFields(log.Fields{"payment_method_id": m.ID, "result_code": update.ResultCode, "change": life.Change}).Info("account updater: card answered")
	}
	return life.Change, err
}

// refused ends a request that will not be answered; stamped when the vault
// itself refused, so the card waits for its next cycle.
func (h *AccountUpdaterHandler) refused(ctx context.Context, m gen.BillingPaymentMethod, reason string, stamp bool) intents.Outcome {
	if stamp {
		if _, err := h.DB.Gen(ctx).StampAccountUpdaterChecked(ctx, gen.StampAccountUpdaterCheckedParams{MerchantID: m.MerchantID, ID: m.ID, CheckedAt: workerNow(h.Clock)}); err != nil {
			return intents.Retryable("record refused account update: " + err.Error())
		}
	}
	log.WithContext(ctx).WithFields(log.Fields{"payment_method_id": m.ID, "reason": reason}).Warn("account updater: request refused; nothing changed")
	return intents.Terminal(reason)
}

// failed retries a request that did not complete, until the attempts run out.
func (h *AccountUpdaterHandler) failed(ctx context.Context, in gen.BillingProviderIntent, m gen.BillingPaymentMethod, reason string) intents.Outcome {
	if in.Attempts < paymentmethods.StoredCardRefreshAttempts {
		return intents.Retryable(reason)
	}
	return h.refused(ctx, m, "gave up: "+reason, true)
}

// Verify never runs: every failure above is retryable under the vault's
// idempotency key.
func (h *AccountUpdaterHandler) Verify(context.Context, gen.BillingProviderIntent) intents.Outcome {
	return intents.Retryable("account updater requests repeat under their idempotency key")
}

const KindStoredCardRefreshDue = "openrails.stored_card_refresh_due"

const (
	// storedCardMerchantBatch caps one pass's merchants; the durable cursor
	// makes those beyond it the next pass's head.
	storedCardMerchantBatch = 200
	// storedCardBatch caps one vault's cards per pass. The lookahead is days
	// wide, so cards beyond it are the next pass's head and still early.
	storedCardBatch = 500
)

type StoredCardRefreshDueArgs struct{}

func (StoredCardRefreshDueArgs) Kind() string { return KindStoredCardRefreshDue }

// StoredCardRefreshDueWorker queues a refresh for each card an armed vault
// holds that pays a renewal inside the vault's lookahead and was not checked
// inside it. Only vaults that must be asked are enumerated, from an index on
// that due work; merchants without an armed vault cost one probe.
type StoredCardRefreshDueWorker struct {
	river.WorkerDefaults[StoredCardRefreshDueArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	Merchants func() *merchants.Service
	// Posture holds the pass for a merchant whose provider writes are held.
	Posture intents.ModeView
	// MerchantBatch / CardBatch override the caps above in tests (0 = default).
	MerchantBatch, CardBatch int
}

func (StoredCardRefreshDueWorker) Kind() string { return KindStoredCardRefreshDue }

func (w StoredCardRefreshDueWorker) Work(ctx context.Context, _ *river.Job[StoredCardRefreshDueArgs]) error {
	_, err := w.RunPass(ctx)
	return err
}

// RunPass queues the due refreshes and returns how many it queued.
func (w StoredCardRefreshDueWorker) RunPass(ctx context.Context) (int, error) {
	if w.DB == nil || w.Merchants == nil || w.Merchants() == nil {
		return 0, errors.New("stored card refresh pass: DB and merchant configuration are required")
	}
	logger := log.WithContext(ctx).WithField("worker", KindStoredCardRefreshDue)
	now := workerNow(w.Clock)
	directory := w.DB.GenDirectory()
	cursorRow, err := loadSweepCursor(ctx, directory, KindStoredCardRefreshDue)
	if err != nil {
		return 0, fmt.Errorf("stored card refresh pass: %w", err)
	}
	cursor := cursorRow.CursorMerchantID
	batch := w.MerchantBatch
	if batch <= 0 {
		batch = storedCardMerchantBatch
	}
	due := func(after *uuid.UUID, limit int32) ([]uuid.UUID, error) {
		return directory.ListStoredCardRefreshMerchants(ctx, gen.ListStoredCardRefreshMerchantsParams{
			After: after, Now: now, MinLookaheadDays: 1, MaxLookaheadDays: int32(custodians.MaxAccountUpdaterLookaheadDays), MerchantLimit: limit,
		})
	}
	merchantIDs, err := due(cursor, clampInt32(batch))
	if err != nil {
		return 0, fmt.Errorf("stored card refresh pass: list merchants: %w", err)
	}
	// The ring wraps inside the pass: what remains goes to the merchants
	// before the cursor.
	if cursor != nil && len(merchantIDs) < batch {
		head, err := due(nil, clampInt32(batch-len(merchantIDs)))
		if err != nil {
			return 0, fmt.Errorf("stored card refresh pass: list merchants: %w", err)
		}
		for _, id := range head {
			if bytes.Compare(id[:], cursor[:]) <= 0 {
				merchantIDs = append(merchantIDs, id)
			}
		}
	}
	var next *uuid.UUID
	if len(merchantIDs) == batch {
		next = &merchantIDs[len(merchantIDs)-1]
	}
	queued := 0
	var passErr error
	for _, id := range merchantIDs {
		progress.Mark(ctx, "stored card refresh merchant "+id.String())
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "stored card refresh pass", func(ctx context.Context) error {
			n, err := w.queueMerchant(ctx, id, now)
			queued += n
			return err
		})
		if err != nil {
			logger.WithError(err).WithField("merchant_id", id).Error("stored card refresh pass: merchant failed; continuing")
			passErr = errors.Join(passErr, err)
		}
	}
	saveSweepCursor(ctx, directory, KindStoredCardRefreshDue, cursorRow, next, logger)
	if queued > 0 {
		logger.WithFields(log.Fields{"merchants": len(merchantIDs), "queued": queued}).Info("stored card refresh pass: refreshes queued")
	}
	return queued, passErr
}

func (w StoredCardRefreshDueWorker) queueMerchant(ctx context.Context, mid uuid.UUID, now time.Time) (int, error) {
	if w.Posture != nil {
		if blocked, _ := intents.GateExecution(ctx, w.Posture, mid, intents.OriginSystem); blocked {
			return 0, nil
		}
	}
	svc := w.Merchants()
	rows, err := svc.ListCustodians(ctx, billing.MerchantID(mid))
	if err != nil {
		return 0, fmt.Errorf("list custodians: %w", err)
	}
	limit := w.CardBatch
	if limit <= 0 {
		limit = storedCardBatch
	}
	queued := 0
	var errs error
	for _, row := range rows {
		if !models.BasisTheoryCustody(row.Kind) || row.Archived {
			continue
		}
		custody, err := svc.BasisTheoryCustodianConfig(ctx, billing.MerchantID(mid), row.ID)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("custodian %s: %w", row.Key, err))
			continue
		}
		if !custody.AccountUpdater {
			continue
		}
		days := custody.AccountUpdaterLookaheadDays
		if days <= 0 {
			days = custodians.DefaultAccountUpdaterLookaheadDays
		}
		// One window, used twice: how far ahead of a renewal a card is
		// checked, and how long that check stays fresh.
		window := time.Duration(days) * 24 * time.Hour
		cards, err := w.DB.Gen(ctx).ListDueStoredCards(ctx, gen.ListDueStoredCardsParams{
			MerchantID: mid, CustodianID: row.ID, StaleBefore: now.Add(-window), RenewalBefore: now.Add(window), RowLimit: clampInt32(limit),
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("custodian %s: list due cards: %w", row.Key, err))
			continue
		}
		if len(cards) == 0 {
			continue
		}
		err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			for _, card := range cards {
				args := paymentmethods.StoredCardRefreshArgs{MerchantID: mid, PaymentMethodID: card.ID, MethodRef: models.DerefStr(card.RailMethodRef), CheckedAt: card.AccountUpdaterCheckedAt}
				opts := args.InsertOpts()
				if err := w.DB.InsertRiverJobTx(ctx, tx, args, &opts); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("custodian %s: queue refreshes: %w", row.Key, err))
			continue
		}
		queued += len(cards)
	}
	return queued, errs
}

func ptrTime(t time.Time) *time.Time { return &t }
