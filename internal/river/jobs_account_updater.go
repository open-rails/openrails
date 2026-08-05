package riverjobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindAccountUpdaterBatch = "openrails.account_updater_batch"

const (
	// accountUpdaterMerchantBatch caps one pass's fan-out. The work queue is
	// indexed on the work itself, so this bounds a pass by ACTIVITY; the
	// durable cursor makes the merchants beyond the cap the NEXT pass's head
	// rather than a starved tail (or#837).
	accountUpdaterMerchantBatch = 200

	// accountUpdaterInstrumentBatch caps ONE batch's membership. Cards beyond
	// it keep their stale watermark and are the next cycle's head — the
	// lookahead window is weeks wide, so nothing misses its renewal over a
	// handful of passes.
	accountUpdaterInstrumentBatch = 2000

	// accountUpdaterOpenBatchCap bounds one merchant's open batches in a pass.
	// The DB already allows only one per custodian; this is the cap that keeps
	// a misconfigured merchant with dozens of custodians from owning the pass.
	accountUpdaterOpenBatchCap = 32

	// accountUpdaterSubmitDeadline abandons a batch that never reached the
	// custodian. It is the vendor's idempotency window: past it a retry could
	// mint a SECOND paid job, so the batch is dropped and its instruments
	// become due again.
	accountUpdaterSubmitDeadline = intents.AccountUpdaterIdempotencyWindow

	// accountUpdaterResultDeadline abandons a submitted batch the custodian
	// never answered. Batch VAU turnaround is days, not weeks; a batch older
	// than this is a support ticket, and holding the open-batch slot forever
	// would block every later cycle.
	accountUpdaterResultDeadline = 14 * 24 * time.Hour
)

type AccountUpdaterBatchArgs struct{}

func (AccountUpdaterBatchArgs) Kind() string { return KindAccountUpdaterBatch }

// AccountUpdaterBatchWorker drives the batch account updater (or#795): the
// ahead-of-renewal cadence that keeps a custodian-held card chargeable through
// a reissue, instead of discovering the reissue as a decline.
//
// Two phases, both activity-shaped:
//
//	INGEST — merchants with an open batch (migration 0076's work queue). Poll
//	  the recorded job; when results are ready, download and fold them through
//	  the EXISTING rotate/park writers. This is what makes a restart safe: a
//	  batch in flight is a durable row with a job ref, so a worker that died
//	  between submit and results resumes by POLLING, never by resubmitting.
//
//	SUBMIT — merchants whose ARMED custodian holds instruments backing
//	  subscriptions that renew inside the lookahead window and whose watermark
//	  is stale. Assemble, write the durable batch, then post a durable intent
//	  that performs the provider write (#674).
//
// Nothing here enumerates payment methods deployment-wide, and a merchant
// without an armed custodian is never visited at all.
type AccountUpdaterBatchWorker struct {
	river.WorkerDefaults[AccountUpdaterBatchArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	Rails  railresolve.Source
	// Intents runs the durable submit (enqueue + inline execute, #674).
	Intents *intents.Runner
	// Outbound governs the provider-supplied result-download url. Zero value
	// is the strict production policy; tests pass loopback.
	Outbound httpx.Policy
	// BTBaseURLOverride points the client at a fake wire server in tests.
	BTBaseURLOverride string
	// MerchantBatch / InstrumentBatch override the caps above (0 = default).
	// They exist so a test can force the capped path without seeding hundreds
	// of merchants; registration never sets them.
	MerchantBatch   int
	InstrumentBatch int
}

func (AccountUpdaterBatchWorker) Kind() string { return KindAccountUpdaterBatch }

// AccountUpdaterPassResult is what one pass did. Work discards it; the tests
// assert on it, because "only the due instruments were batched" is not
// observable from the instrument rows alone.
type AccountUpdaterPassResult struct {
	IngestMerchants    []uuid.UUID
	SubmitMerchants    []uuid.UUID
	BatchesCreated     int
	BatchesSubmitted   int
	BatchesCompleted   int
	BatchesAbandoned   int
	InstrumentsBatched int
	Folded             webhooks.AccountUpdaterFoldStats
}

func (w AccountUpdaterBatchWorker) Work(ctx context.Context, _ *river.Job[AccountUpdaterBatchArgs]) error {
	_, err := w.RunPass(ctx)
	return err
}

// RunPass is Work's body, returning what the pass did.
func (w AccountUpdaterBatchWorker) RunPass(ctx context.Context) (AccountUpdaterPassResult, error) {
	var result AccountUpdaterPassResult
	if w.DB == nil || w.Rails == nil {
		// Fail loudly rather than run a pass that can arm no custodian and would
		// report a comfortable "nothing due".
		return result, errors.New("account updater: DB and custodian resolution are required")
	}
	logger := log.WithContext(ctx).WithField("worker", KindAccountUpdaterBatch)
	now := workerNow(w.Clock)
	var passErr error

	// --- INGEST: results first. A merchant with an open batch has no new work
	// (the DB allows one open batch per custodian), so closing it is what
	// unblocks the next cycle.
	directory := w.DB.GenDirectory()
	openMerchants, err := directory.ListAccountUpdaterOpenBatchMerchants(ctx, clampInt32(w.merchantBatch()))
	if err != nil {
		return result, fmt.Errorf("account updater: list merchants with open batches: %w", err)
	}
	for _, mid := range openMerchants {
		if mid == nil {
			continue
		}
		merchantID := *mid
		result.IngestMerchants = append(result.IngestMerchants, merchantID)
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(merchantID), "account updater ingest", func(mctx context.Context) error {
			return w.ingestMerchant(mctx, merchantID, now, &result)
		}); err != nil {
			// One merchant's failure must not abort the rest of the fan-out.
			logger.WithError(err).WithField("merchant_id", merchantID).Error("Account updater: ingest pass failed; continuing")
			passErr = errors.Join(passErr, err)
		}
	}

	// --- SUBMIT: new cycles.
	cursor, err := directory.GetSweepCursor(ctx, KindAccountUpdaterBatch)
	if err != nil && !db.IsNotFound(err) {
		return result, fmt.Errorf("account updater: load sweep cursor: %w", err)
	}
	batch := w.merchantBatch()
	dueWork := func(after *uuid.UUID, limit int32) ([]*uuid.UUID, error) {
		return directory.ListAccountUpdaterWorkMerchants(ctx, gen.ListAccountUpdaterWorkMerchantsParams{
			Custodian:            models.CustodianBasisTheory,
			Environment:          w.environment(),
			Now:                  now,
			DefaultLookaheadDays: int32(custodians.DefaultAccountUpdaterLookaheadDays),
			After:                after,
			MerchantLimit:        limit,
		})
	}
	merchantIDs, err := dueWork(cursor, clampInt32(batch))
	if err != nil {
		return result, fmt.Errorf("account updater: list merchants with due instruments: %w", err)
	}
	// The ring wraps INSIDE the pass, like the retention sweep: a short list
	// means everything after the cursor is drained, so the rest of this pass's
	// capacity goes to the merchants before it.
	if cursor != nil && len(merchantIDs) < batch {
		head, herr := dueWork(nil, clampInt32(batch-len(merchantIDs)))
		if herr != nil {
			return result, fmt.Errorf("account updater: list merchants with due instruments (ring wrap): %w", herr)
		}
		for _, mid := range head {
			if mid != nil && bytes.Compare(mid[:], cursor[:]) <= 0 {
				merchantIDs = append(merchantIDs, mid)
			}
		}
	}
	var nextCursor *uuid.UUID
	if len(merchantIDs) == batch {
		nextCursor = merchantIDs[len(merchantIDs)-1]
	}

	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := *mid
		result.SubmitMerchants = append(result.SubmitMerchants, merchantID)
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(merchantID), "account updater submit", func(mctx context.Context) error {
			return w.submitMerchant(mctx, merchantID, now, &result)
		}); err != nil {
			logger.WithError(err).WithField("merchant_id", merchantID).Error("Account updater: submit pass failed; continuing")
			passErr = errors.Join(passErr, err)
		}
	}

	if serr := directory.SaveSweepCursor(ctx, gen.SaveSweepCursorParams{
		WorkerKind: KindAccountUpdaterBatch, CursorMerchantID: nextCursor,
	}); serr != nil {
		// A lost cursor costs fairness on the next pass, not correctness.
		logger.WithError(serr).Warn("Account updater: could not persist sweep cursor")
	}

	if result.BatchesCreated > 0 || result.BatchesCompleted > 0 || result.BatchesAbandoned > 0 {
		logger.WithFields(log.Fields{
			"merchants_ingested":  len(result.IngestMerchants),
			"merchants_submitted": len(result.SubmitMerchants),
			"batches_created":     result.BatchesCreated,
			"batches_submitted":   result.BatchesSubmitted,
			"batches_completed":   result.BatchesCompleted,
			"batches_abandoned":   result.BatchesAbandoned,
			"instruments_batched": result.InstrumentsBatched,
			"adopted":             result.Folded.Adopted,
			"parked":              result.Folded.Parked,
			"more_work_queued":    nextCursor != nil,
		}).Info("Account updater: pass complete")
	}
	if passErr != nil {
		return result, fmt.Errorf("account updater: %w", passErr)
	}
	return result, nil
}

func (w AccountUpdaterBatchWorker) merchantBatch() int {
	if w.MerchantBatch > 0 {
		return w.MerchantBatch
	}
	return accountUpdaterMerchantBatch
}

func (w AccountUpdaterBatchWorker) instrumentBatch() int {
	if w.InstrumentBatch > 0 {
		return w.InstrumentBatch
	}
	return accountUpdaterInstrumentBatch
}

func (w AccountUpdaterBatchWorker) environment() string {
	return config.ExpectedProviderEnvironment(w.Config != nil && w.Config.IsTestMode())
}

// ingestMerchant polls this merchant's open batches and folds whatever the
// custodian has finished. Already inside the merchant's scope.
func (w AccountUpdaterBatchWorker) ingestMerchant(ctx context.Context, mid uuid.UUID, now time.Time, result *AccountUpdaterPassResult) error {
	q := w.DB.Gen(ctx)
	batches, err := q.ListOpenAccountUpdaterBatches(ctx, gen.ListOpenAccountUpdaterBatchesParams{
		MerchantID: mid,
		// One open batch per custodian is a DB invariant; the cap is the
		// belt-and-braces bound on one merchant's share of a pass.
		RowLimit: int64(accountUpdaterOpenBatchCap),
	})
	if err != nil {
		return fmt.Errorf("list open account updater batches: %w", err)
	}
	logger := log.WithContext(ctx).WithFields(log.Fields{"worker": KindAccountUpdaterBatch, "merchant_id": mid})
	var errs error
	for _, batch := range batches {
		jobRef := strings.TrimSpace(batch.JobRef)
		if batch.Status == "pending" || jobRef == "" {
			// Never reached the custodian. The submit intent retries it; past
			// the vendor's idempotency window a retry could pay twice, so the
			// batch is abandoned and its instruments simply become due again.
			if now.Sub(batch.CreatedAt) > accountUpdaterSubmitDeadline {
				if err := w.abandon(ctx, mid, batch.ID, now, "submit never confirmed inside the custodian's idempotency window"); err != nil {
					errs = errors.Join(errs, err)
					continue
				}
				result.BatchesAbandoned++
				logger.WithField("batch_id", batch.ID).Error("Account updater: batch abandoned unsubmitted; its instruments become due again (nothing parked — no evidence about any card)")
			}
			continue
		}

		client, err := w.client(ctx, batch.CustodianID)
		if err != nil {
			// An unarmed custodian is not a batch failure: leave the row open
			// and try again next pass.
			logger.WithError(err).WithField("batch_id", batch.ID).Warn("Account updater: custodian not armed; leaving batch open")
			continue
		}
		job, err := client.GetAccountUpdaterJob(ctx, jobRef)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("poll account updater job %s: %w", jobRef, err))
			continue
		}
		if _, err := q.MarkAccountUpdaterBatchPolled(ctx, gen.MarkAccountUpdaterBatchPolledParams{
			MerchantID: mid, ID: batch.ID, PolledAt: now,
		}); err != nil {
			errs = errors.Join(errs, fmt.Errorf("record poll for batch %s: %w", batch.ID, err))
		}
		if strings.TrimSpace(job.DownloadURL) == "" {
			// Still working. Abandon only if the custodian has gone silent for
			// far longer than a batch can honestly take.
			if batch.SubmittedAt != nil && now.Sub(*batch.SubmittedAt) > accountUpdaterResultDeadline {
				if err := w.abandon(ctx, mid, batch.ID, now, "custodian returned no results within "+accountUpdaterResultDeadline.String()); err != nil {
					errs = errors.Join(errs, err)
					continue
				}
				result.BatchesAbandoned++
				logger.WithFields(log.Fields{"batch_id": batch.ID, "job_ref": jobRef}).
					Error("Account updater: batch abandoned without results; operator action required (no card was parked — silence is not evidence)")
			}
			continue
		}

		rows, err := client.DownloadAccountUpdaterResults(ctx, job.DownloadURL)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("download account updater results for job %s: %w", jobRef, err))
			continue
		}
		// The ONE fold: rotate through RotateCustodianMethodRef (which clears
		// the park, or#872), park closed/contact-cardholder, record every code
		// verbatim. The webhook path lands in the same function.
		stats, err := webhooks.FoldAccountUpdaterResults(ctx, q, rows)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("fold account updater results for job %s: %w", jobRef, err))
			continue
		}
		if err := webhooks.CloseAccountUpdaterBatch(ctx, q, jobRef, stats); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		result.BatchesCompleted++
		result.Folded.Rows += stats.Rows
		result.Folded.Adopted += stats.Adopted
		result.Folded.Rotated += stats.Rotated
		result.Folded.Parked += stats.Parked
	}
	return errs
}

func (w AccountUpdaterBatchWorker) abandon(ctx context.Context, mid, batchID uuid.UUID, now time.Time, reason string) error {
	_, err := w.DB.Gen(ctx).FailAccountUpdaterBatch(ctx, gen.FailAccountUpdaterBatchParams{
		MerchantID: mid, ID: batchID, FailureReason: reason, CompletedAt: now,
	})
	if err != nil {
		return fmt.Errorf("abandon account updater batch %s: %w", batchID, err)
	}
	return nil
}

// submitMerchant assembles and submits one batch per armed custodian. Already
// inside the merchant's scope.
func (w AccountUpdaterBatchWorker) submitMerchant(ctx context.Context, mid uuid.UUID, now time.Time, result *AccountUpdaterPassResult) error {
	q := w.DB.Gen(ctx)
	rows, err := q.ListCustodiansForMerchant(ctx, mid)
	if err != nil {
		return fmt.Errorf("list custodians: %w", err)
	}
	env := w.environment()
	logger := log.WithContext(ctx).WithFields(log.Fields{"worker": KindAccountUpdaterBatch, "merchant_id": mid})
	var errs error
	for _, row := range rows {
		if row.Kind != models.CustodianBasisTheory || row.Archived || row.Environment != env {
			continue
		}
		cc, err := w.Rails.CustodianConfig(ctx, row.Kind, row.AccountID)
		if err != nil || cc == nil {
			logger.WithError(err).WithField("custodian", row.Key).Warn("Account updater: custodian not armed; skipping")
			continue
		}
		if !cc.AccountUpdater {
			continue
		}
		// One window, used twice: how far ahead of a renewal a card is
		// refreshed, and how long that refresh stays fresh. A resolver that
		// handed back no window falls back to the same default the settings
		// parser and the work queue use — never to zero, which would select
		// nothing and read as "no work".
		days := cc.AccountUpdaterLookaheadDays
		if days <= 0 {
			days = custodians.DefaultAccountUpdaterLookaheadDays
		}
		window := time.Duration(days) * 24 * time.Hour
		due, err := q.ListDueAccountUpdaterInstruments(ctx, gen.ListDueAccountUpdaterInstrumentsParams{
			MerchantID:    mid,
			Custodian:     row.Kind,
			StaleBefore:   now.Add(-window),
			RenewalBefore: now.Add(window),
			RowLimit:      int64(w.instrumentBatch()),
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("list due instruments for custodian %s: %w", row.Key, err))
			continue
		}
		if len(due) == 0 {
			continue
		}
		instruments := make([]intents.AccountUpdaterBatchInstrument, 0, len(due))
		for _, in := range due {
			month, year := splitInstrumentExpiry(in.ExpiryDate)
			instruments = append(instruments, intents.AccountUpdaterBatchInstrument{
				PaymentMethodID: in.ID,
				Token:           in.RailMethodRef,
				ExpirationMonth: month,
				ExpirationYear:  year,
			})
		}
		payload, err := json.Marshal(instruments)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("encode batch membership: %w", err))
			continue
		}
		batch, err := q.CreateAccountUpdaterBatch(ctx, gen.CreateAccountUpdaterBatchParams{
			MerchantID: mid, CustodianID: row.ID, Instruments: payload,
		})
		if err != nil {
			// The one-open-batch-per-custodian index: another node won the
			// race. Not an error — that node owns this cycle.
			if isUniqueViolation(err) {
				continue
			}
			errs = errors.Join(errs, fmt.Errorf("create account updater batch: %w", err))
			continue
		}
		result.BatchesCreated++
		result.InstrumentsBatched += len(instruments)

		if w.Intents == nil {
			errs = errors.Join(errs, errors.New("account updater: intent runner is not wired; batch assembled but not submitted"))
			continue
		}
		// #674 write-through: the provider write goes through the durable
		// intent, whose key also becomes the custodian's idempotency key.
		intent, err := w.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
			MerchantID: mid,
			Provider:   models.CustodianBasisTheory,
			IntentType: intents.TypeAccountUpdaterBatchSubmit,
			Payload: intents.AccountUpdaterBatchPayload{
				BatchID:            batch.ID,
				CustodianKind:      row.Kind,
				CustodianAccountID: row.AccountID,
			},
			IdempotencyKey: intents.AccountUpdaterBatchIdempotencyKey(batch.ID),
			NextAttemptAt:  now,
			// The relevance window IS the vendor's idempotency window: past it
			// a retry stops being free, so the intent expires instead.
			ExpiresAt:    ptrTime(now.Add(intents.AccountUpdaterIdempotencyWindow)),
			Origin:       intents.OriginSystem,
			OriginReason: "batch account updater: ahead-of-renewal refresh",
		})
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("submit account updater batch %s: %w", batch.ID, err))
			continue
		}
		if intent.Status == intents.StatusSucceeded {
			result.BatchesSubmitted++
		}
	}
	return errs
}

func (w AccountUpdaterBatchWorker) client(ctx context.Context, custodianID uuid.UUID) (*basistheory.Client, error) {
	if w.Rails == nil {
		return nil, errors.New("custodian resolution is not configured")
	}
	row, err := w.DB.Gen(ctx).GetCustodian(ctx, custodianID)
	if err != nil {
		return nil, fmt.Errorf("load custodian %s: %w", custodianID, err)
	}
	cc, err := w.Rails.CustodianConfig(ctx, row.Kind, row.AccountID)
	if err != nil {
		return nil, err
	}
	if cc == nil {
		return nil, fmt.Errorf("custodian %s/%s resolved to nothing", row.Kind, row.AccountID)
	}
	baseURL := w.BTBaseURLOverride
	if baseURL == "" {
		baseURL = cc.APIBaseURL
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cc.APIKey,
		BaseURL:       baseURL,
		WebhookKeyURL: cc.WebhookKeyURL,
		ReadOnly:      w.Config != nil && w.Config.IsProviderReadOnly(),
		Outbound:      w.Outbound,
	})
}

// splitInstrumentExpiry turns the stored MM/YY into the request CSV's optional
// month/year columns. An unparseable or absent expiry yields empty strings —
// both columns are optional, and inventing one would be a fabricated hint to
// the network (#651).
func splitInstrumentExpiry(expiry *string) (month, year string) {
	if expiry == nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimSpace(*expiry), "/", 2)
	if len(parts) != 2 {
		return "", ""
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || m < 1 || m > 12 {
		return "", ""
	}
	y, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || y < 0 {
		return "", ""
	}
	if y < 100 {
		y += 2000
	}
	return fmt.Sprintf("%02d", m), strconv.Itoa(y)
}

func ptrTime(t time.Time) *time.Time { return &t }

// isUniqueViolation: SQLSTATE 23505, checked structurally so it survives
// message changes.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
