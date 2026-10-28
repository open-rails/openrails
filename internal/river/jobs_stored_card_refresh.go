package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
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

// Operator findings the account updater raises.
const (
	// FindingAccountUpdaterUnauthorized: the vault refused the merchant's
	// key. Its ahead-of-renewal checks wait until the vault answers again or
	// the finding is resolved.
	FindingAccountUpdaterUnauthorized = "card.account_updater.unauthorized"
	// FindingAccountUpdateUnapplied: an answer could not be applied to the
	// card; the card is left as it was and checked again next cycle.
	FindingAccountUpdateUnapplied = "card.account_updater.unapplied"
)

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
	final := job.Attempt >= paymentmethods.StoredCardRefreshAttempts
	return w.DB.RunInMerchantScope(ctx, billing.MerchantID(args.MerchantID), "stored card refresh", func(ctx context.Context) error {
		err := w.refresh(ctx, args, final)
		if err != nil && args.Declined && final {
			// A declined card's customer always hears, whatever failed.
			log.WithContext(ctx).WithError(err).WithField("payment_method_id", args.PaymentMethodID).Warn("stored card refresh: gave up; asking the customer")
			return w.askUnlessReissued(ctx, args)
		}
		return err
	})
}

func (w *StoredCardRefreshWorker) refresh(ctx context.Context, args paymentmethods.StoredCardRefreshArgs, final bool) error {
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
	if reissued, checked := args.Moot(method); reissued || checked {
		if !args.Declined {
			return nil
		}
		// Overtaken since the decline: a reissued card retries its
		// renewals; one checked without a new card asks the customer.
		if !reissued {
			return w.askUnlessReissued(ctx, args)
		}
		return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return subscriptions.WakeForReplacedMethod(ctx, w.DB.NewWithPgxTx(tx), args.MerchantID, method.ID, now)
		})
	}
	_, armed, err := w.Updaters.AccountUpdater(ctx, method)
	if err != nil {
		return fmt.Errorf("stored card refresh: resolve holder: %w", err)
	}
	if armed {
		if blocked, reason := intents.GateExecution(ctx, w.Intents.Config, args.MerchantID, intents.OriginSystem); blocked {
			log.WithContext(ctx).WithFields(log.Fields{"payment_method_id": method.ID, "reason": reason}).Info("stored card refresh: provider writes are held")
			armed = false
		}
	}
	if !armed {
		if !args.Declined {
			return nil
		}
		return w.askUnlessReissued(ctx, args)
	}
	intent, err := w.request(ctx, method, args.Declined, now)
	if err != nil || !args.Declined {
		return err
	}
	// A declined card's customer is asked unless the request reissued it. A
	// request still running is waited on, never past the job's attempts.
	if !intents.OperationTerminal(intent.Status) && !final {
		return fmt.Errorf("stored card refresh: account update %s is %s", intent.ID, intent.Status)
	}
	return w.askUnlessReissued(ctx, args)
}

// request records the card's account-updater request and runs its first
// attempt. One attempted already runs on its own backoff.
func (w *StoredCardRefreshWorker) request(ctx context.Context, method gen.BillingPaymentMethod, declined bool, now time.Time) (gen.BillingProviderIntent, error) {
	ref := models.DerefStr(method.RailMethodRef)
	checked := "never"
	if method.AccountUpdaterCheckedAt != nil {
		checked = strconv.FormatInt(method.AccountUpdaterCheckedAt.UnixNano(), 10)
	}
	cause := "renewal"
	if declined {
		cause = "decline"
	}
	row, err := w.Intents.Store.Enqueue(ctx, intents.EnqueueParams{
		MerchantID:  method.MerchantID,
		Provider:    method.Custodian,
		CustodianID: *method.CustodianID,
		IntentType:  TypeAccountUpdater,
		Payload:     accountUpdaterPayload{PaymentMethodID: method.ID, RailMethodRef: ref},
		// One request per card, handle, last check and day: a retried job, a
		// re-queued card or a decline beside a renewal check share it, and a
		// request that expired unanswered does not block the next day's.
		IdempotencyKey: TypeAccountUpdater + ":" + method.ID.String() + ":" + ref + ":" + checked + ":" + now.UTC().Format(time.DateOnly),
		NextAttemptAt:  now,
		ExpiresAt:      ptrTime(now.Add(accountUpdaterWindow)),
		Origin:         intents.OriginSystem,
		OriginReason:   "stored card refresh: " + cause,
		Inline:         true,
	})
	if err != nil {
		return row, err
	}
	return w.Intents.ExecuteFirstByID(ctx, row.ID)
}

// askUnlessReissued asks for a card still as the job found it; a reissue
// already woke the renewals waiting on it, and a closed card asked already.
func (w *StoredCardRefreshWorker) askUnlessReissued(ctx context.Context, args paymentmethods.StoredCardRefreshArgs) error {
	now := workerNow(w.Clock)
	return w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		current, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: args.MerchantID, ID: args.PaymentMethodID})
		if db.IsNotFound(err) {
			return nil
		}
		if err != nil || current.Status != paymentmethods.StatusActive || args.Card != "" && paymentmethods.CardVersion(current) != args.Card {
			return err
		}
		_, err = w.Lifecycle.AskForCard(ctx, d, args.MerchantID, args.PaymentMethodID, now)
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

// Execute asks the vault once. A transient failure (timeout, throttling,
// 5xx, transport) retries on the intent's backoff under the same key until the
// vault's idempotency window closes; it never marks the card checked.
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
		return intents.Retryable("resolve holder: " + err.Error())
	}
	if !armed {
		return intents.Terminal("the card's holder is no longer armed for updates")
	}
	update, err := updater.Update(ctx, in.ID.String())
	switch {
	case errors.Is(err, basistheory.ErrProviderReadOnly):
		return intents.Parked("vault writes are blocked (mode=readonly)")
	case errors.Is(err, paymentmethods.ErrAccountUpdaterUnauthorized):
		return h.holdVault(ctx, m, err)
	case errors.Is(err, paymentmethods.ErrAccountUpdateRefused):
		if err := h.stamp(ctx, m); err != nil {
			return intents.Retryable("record refused account update: " + err.Error())
		}
		log.WithContext(ctx).WithError(err).WithField("payment_method_id", m.ID).Warn("account updater: request refused; nothing changed")
		return intents.Terminal(err.Error())
	case err != nil:
		return intents.Retryable(err.Error())
	}
	if _, err := h.DB.Gen(ctx).ResolveStoredCardVaultHold(ctx, gen.ResolveStoredCardVaultHoldParams{MerchantID: m.MerchantID, FindingType: FindingAccountUpdaterUnauthorized, SubjectKey: m.CustodianID.String()}); err != nil {
		log.WithContext(ctx).WithError(err).Warn("account updater: could not resolve the vault's refused-key finding")
	}
	change, err := h.apply(ctx, in, m, update)
	if err != nil {
		// The vault answers a repeat of this key with the same result; an
		// answer that keeps failing is left for an operator.
		if in.Attempts < paymentmethods.StoredCardRefreshAttempts {
			return intents.Retryable("apply account update: " + err.Error())
		}
		return h.unapplied(ctx, m, update, err)
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

func (h *AccountUpdaterHandler) stamp(ctx context.Context, m gen.BillingPaymentMethod) error {
	_, err := h.DB.Gen(ctx).StampAccountUpdaterChecked(ctx, gen.StampAccountUpdaterCheckedParams{MerchantID: m.MerchantID, ID: m.ID, CheckedAt: workerNow(h.Clock)})
	return err
}

// holdVault raises the vault's one refused-key finding; no card is marked
// checked, and the pass holds the vault's checks while the finding is open.
func (h *AccountUpdaterHandler) holdVault(ctx context.Context, m gen.BillingPaymentMethod, cause error) intents.Outcome {
	evidence, _ := json.Marshal(map[string]any{"custodian_id": m.CustodianID, "custodian": m.Custodian, "error": cause.Error()})
	action := "the card vault refused the merchant's key for the Account Updater; fix the key or its account-updater:real-time:invoke permission, then resolve this finding"
	if _, err := h.DB.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: m.MerchantID, FindingType: FindingAccountUpdaterUnauthorized,
		SubjectKey: m.CustodianID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
		return intents.Retryable("record refused vault key: " + err.Error())
	}
	log.WithContext(ctx).WithError(cause).WithField("custodian_id", m.CustodianID).Error("account updater: the vault refused the merchant's key; its checks are held")
	return intents.Terminal(cause.Error())
}

// unapplied gives up on an answer that cannot be applied: the card is marked
// checked so the pass waits for its next cycle, and an operator is told.
func (h *AccountUpdaterHandler) unapplied(ctx context.Context, m gen.BillingPaymentMethod, update paymentmethods.AccountUpdate, cause error) intents.Outcome {
	evidence, _ := json.Marshal(map[string]any{"payment_method_id": m.ID, "result_code": update.ResultCode, "error": cause.Error()})
	action := "the card vault reported a change to this card that could not be applied; review the card and its vault token"
	err := h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.StampAccountUpdaterChecked(ctx, gen.StampAccountUpdaterCheckedParams{MerchantID: m.MerchantID, ID: m.ID, CheckedAt: workerNow(h.Clock)}); err != nil {
			return err
		}
		_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: m.MerchantID, FindingType: FindingAccountUpdateUnapplied,
			SubjectKey: m.ID.String(), Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
		return err
	})
	if err != nil {
		return intents.Retryable("record unapplied account update: " + err.Error())
	}
	log.WithContext(ctx).WithError(cause).WithField("payment_method_id", m.ID).Error("account updater: answer could not be applied; operator finding raised")
	return intents.Terminal("unapplied: " + cause.Error())
}

// Verify never runs: every failure above is retryable under the vault's
// idempotency key.
func (h *AccountUpdaterHandler) Verify(context.Context, gen.BillingProviderIntent) intents.Outcome {
	return intents.Retryable("account updater requests repeat under their idempotency key")
}

const KindStoredCardRefreshDue = "openrails.stored_card_refresh_due"

const (
	// storedCardBatch is one page of each kind of due check: cards marked for
	// a look and planned checks are read side by side, so neither starves the
	// other.
	storedCardBatch = 500
	// storedCardPassBudget bounds one pass: it opens no page past it, and the
	// next pass reads what is left.
	storedCardPassBudget = 10 * time.Minute
	// storedCardJitter spreads one pass's requests over the hour after it.
	storedCardJitter = time.Hour
	// storedCardReplan is when a card queued for a refresh is looked at again:
	// checked by then it waits for its next renewal, otherwise it is queued
	// again while its window lasts.
	storedCardReplan = 24 * time.Hour
	// storedCardVaultRetry postpones a vault whose configuration could not be
	// read, or whose writes are held.
	storedCardVaultRetry = time.Hour
)

type StoredCardRefreshDueArgs struct{}

func (StoredCardRefreshDueArgs) Kind() string { return KindStoredCardRefreshDue }

// StoredCardRefreshDueWorker queues a refresh for each card an armed vault
// holds whose next renewal is inside the vault's lookahead and that was not
// checked for it. It reads only billing.stored_card_checks: a subscription's
// period, status or card change marks its card, and a look either plans the
// card's next check, queues it, or drops it until its renewals change again.
// A vault armed later is first asked about a card ahead of its next renewal.
type StoredCardRefreshDueWorker struct {
	river.WorkerDefaults[StoredCardRefreshDueArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	Merchants func() *merchants.Service
	// Posture holds a merchant's checks while its provider writes are held.
	Posture intents.ModeView
}

func (StoredCardRefreshDueWorker) Kind() string { return KindStoredCardRefreshDue }

func (w StoredCardRefreshDueWorker) Work(ctx context.Context, _ *river.Job[StoredCardRefreshDueArgs]) error {
	_, err := w.RunPass(ctx)
	return err
}

// RunPass looks at the due checks and returns how many refreshes it queued.
// Each round reads a page of marked cards and a page of planned checks, so
// neither starves the other, until both run short or the budget is spent.
func (w StoredCardRefreshDueWorker) RunPass(ctx context.Context) (int, error) {
	if w.DB == nil || w.Merchants == nil || w.Merchants() == nil {
		return 0, errors.New("stored card refresh pass: DB and merchant configuration are required")
	}
	logger := log.WithContext(ctx).WithField("worker", KindStoredCardRefreshDue)
	now := workerNow(w.Clock)
	q := w.DB.GenDirectory()
	failed := map[uuid.UUID]bool{}
	var after gen.ListPlannedStoredCardChecksParams
	queued, looked := 0, 0
	markedDone, plannedDone := false, false
	var passErr error
	for !(markedDone && plannedDone) && workerNow(w.Clock).Sub(now) < storedCardPassBudget {
		var page []dueCheck
		if !markedDone {
			skip := make([]uuid.UUID, 0, len(failed))
			for id := range failed {
				skip = append(skip, id)
			}
			rows, err := q.ListMarkedStoredCardChecks(ctx, gen.ListMarkedStoredCardChecksParams{SkipMerchantIds: skip, RowLimit: storedCardBatch})
			if err != nil {
				return queued, errors.Join(passErr, fmt.Errorf("stored card refresh pass: list marked cards: %w", err))
			}
			markedDone = len(rows) < storedCardBatch
			for _, row := range rows {
				page = append(page, dueCheck{row.MerchantID, row.PaymentMethodID})
			}
		}
		if !plannedDone {
			params := after
			params.Now, params.RowLimit = now, storedCardBatch
			rows, err := q.ListPlannedStoredCardChecks(ctx, params)
			if err != nil {
				return queued, errors.Join(passErr, fmt.Errorf("stored card refresh pass: list planned checks: %w", err))
			}
			plannedDone = len(rows) < storedCardBatch
			for _, row := range rows {
				page = append(page, dueCheck{row.MerchantID, row.PaymentMethodID})
				after.AfterDueAt, after.AfterMerchantID, after.AfterPaymentMethodID = row.DueAt, row.MerchantID, row.PaymentMethodID
			}
		}
		looked += len(page)
		n, err := w.lookAt(ctx, page, now, failed)
		queued += n
		passErr = errors.Join(passErr, err)
	}
	if queued > 0 {
		logger.WithFields(log.Fields{"checks": looked, "queued": queued}).Info("stored card refresh pass: refreshes queued")
	}
	return queued, passErr
}

// dueCheck is one card due for a look.
type dueCheck struct{ merchant, card uuid.UUID }

// lookAt looks at one page of due checks, merchant by merchant. A merchant
// that fails is recorded in failed and skipped for the rest of the pass.
func (w StoredCardRefreshDueWorker) lookAt(ctx context.Context, due []dueCheck, now time.Time, failed map[uuid.UUID]bool) (int, error) {
	logger := log.WithContext(ctx).WithField("worker", KindStoredCardRefreshDue)
	var order []uuid.UUID
	cards := map[uuid.UUID][]uuid.UUID{}
	for _, check := range due {
		if failed[check.merchant] {
			continue
		}
		if _, seen := cards[check.merchant]; !seen {
			order = append(order, check.merchant)
		}
		cards[check.merchant] = append(cards[check.merchant], check.card)
	}
	queued := 0
	var passErr error
	for _, mid := range order {
		progress.Mark(ctx, "stored card refresh merchant "+mid.String())
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(mid), "stored card refresh pass", func(ctx context.Context) error {
			n, err := w.lookAtMerchant(ctx, mid, cards[mid], now)
			queued += n
			return err
		})
		if err != nil {
			failed[mid] = true
			logger.WithError(err).WithField("merchant_id", mid).Error("stored card refresh pass: merchant failed; continuing")
			passErr = errors.Join(passErr, err)
		}
	}
	return queued, passErr
}

// vaultPlan is what one pass knows about a vault.
type vaultPlan struct {
	armed  bool
	window time.Duration
	// retryAt postpones the vault's checks: unreadable configuration, held
	// writes or a refused key.
	retryAt *time.Time
}

func (w StoredCardRefreshDueWorker) vault(ctx context.Context, mid, custodianID uuid.UUID, now time.Time) vaultPlan {
	logger := log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "custodian_id": custodianID})
	later := func(d time.Duration) vaultPlan { at := now.Add(d); return vaultPlan{retryAt: &at} }
	if w.Posture != nil {
		if blocked, _ := intents.GateExecution(ctx, w.Posture, mid, intents.OriginSystem); blocked {
			return later(storedCardVaultRetry)
		}
	}
	custody, err := w.Merchants().BasisTheoryCustodianConfig(ctx, billing.MerchantID(mid), custodianID)
	switch {
	case errors.Is(err, merchants.ErrCustodianNotDeclared) || errors.Is(err, merchants.ErrPlatformVaultUnavailable):
		return vaultPlan{}
	case err != nil:
		logger.WithError(err).Warn("stored card refresh pass: vault configuration unreadable; retrying later")
		return later(storedCardVaultRetry)
	case !custody.AccountUpdater:
		return vaultPlan{}
	}
	held, err := w.DB.Gen(ctx).StoredCardVaultHeld(ctx, gen.StoredCardVaultHeldParams{MerchantID: mid, FindingType: FindingAccountUpdaterUnauthorized, SubjectKey: custodianID.String()})
	if err != nil || held {
		return later(storedCardReplan)
	}
	days := custody.AccountUpdaterLookaheadDays
	if days <= 0 {
		days = custodians.DefaultAccountUpdaterLookaheadDays
	}
	return vaultPlan{armed: true, window: time.Duration(days) * 24 * time.Hour}
}

func (w StoredCardRefreshDueWorker) lookAtMerchant(ctx context.Context, mid uuid.UUID, ids []uuid.UUID, now time.Time) (int, error) {
	q := w.DB.Gen(ctx)
	vaults := map[uuid.UUID]vaultPlan{}
	drop := func(id uuid.UUID) error {
		_, err := q.DeleteStoredCardCheck(ctx, gen.DeleteStoredCardCheckParams{MerchantID: mid, PaymentMethodID: id})
		return err
	}
	plan := func(id uuid.UUID, at time.Time) error {
		_, err := q.PlanStoredCardCheck(ctx, gen.PlanStoredCardCheckParams{MerchantID: mid, PaymentMethodID: id, DueAt: at})
		return err
	}
	queued := 0
	for _, id := range ids {
		m, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid, ID: id})
		if db.IsNotFound(err) {
			continue // the check went with the card
		}
		if err != nil {
			return queued, err
		}
		if m.Status != paymentmethods.StatusActive || !paymentmethods.AsksForUpdates(m) {
			if err := drop(id); err != nil {
				return queued, err
			}
			continue
		}
		v, seen := vaults[*m.CustodianID]
		if !seen {
			v = w.vault(ctx, mid, *m.CustodianID, now)
			vaults[*m.CustodianID] = v
		}
		if v.retryAt != nil {
			if err := plan(id, *v.retryAt); err != nil {
				return queued, err
			}
			continue
		}
		renewals, err := q.StoredCardNextRenewal(ctx, gen.StoredCardNextRenewalParams{MerchantID: mid, PaymentMethodID: id})
		if err != nil {
			return queued, err
		}
		if !v.armed || len(renewals) == 0 {
			// Unarmed or paying nothing: the next renewal change marks it again.
			if err := drop(id); err != nil {
				return queued, err
			}
			continue
		}
		opens := renewals[0].Add(-v.window)
		switch {
		case opens.After(now):
			err = plan(id, opens)
		case m.AccountUpdaterCheckedAt != nil && !m.AccountUpdaterCheckedAt.Before(opens):
			err = drop(id) // checked for this renewal
		default:
			err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				args := paymentmethods.StoredCardRefreshFor(m, false, "")
				opts := args.InsertOpts()
				opts.ScheduledAt = now.Add(jitter(id))
				if _, err := w.DB.InsertRiverJobTx(ctx, tx, args, &opts); err != nil {
					return err
				}
				_, err := gen.New(tx).PlanStoredCardCheck(ctx, gen.PlanStoredCardCheckParams{MerchantID: mid, PaymentMethodID: id, DueAt: now.Add(storedCardReplan)})
				return err
			})
			if err == nil {
				queued++
			}
		}
		if err != nil {
			return queued, err
		}
	}
	return queued, nil
}

// jitter spreads a pass's requests over storedCardJitter, the same offset for
// a card every time.
func jitter(id uuid.UUID) time.Duration {
	h := fnv.New64a()
	_, _ = h.Write(id[:])
	return time.Duration(h.Sum64() % uint64(storedCardJitter))
}

func ptrTime(t time.Time) *time.Time { return &t }
