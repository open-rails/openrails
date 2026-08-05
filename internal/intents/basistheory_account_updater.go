package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/httpx"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TypeAccountUpdaterBatchSubmit is the durable submit half of the batch
// account updater (or#795). Creating the job and uploading the token CSV are
// provider WRITES — a paid one — so they go through the intent ledger like
// every other outbound mutation (#674) instead of being fired from a worker
// loop that a restart repeats.
//
// Effectively-once rests on two things, not on hope:
//
//   - the durable batch row (openrails.account_updater_batches) is written
//     BEFORE the provider is touched and records the job id the moment the
//     create is confirmed, so a resumed attempt polls that job;
//   - the create carries an intent-derived BT-IDEMPOTENCY-KEY, so even a
//     create whose RESPONSE was lost returns the same job on retry. That is
//     why a failed create is Retryable rather than Ambiguous: there is no
//     ambiguity left for a verifier to resolve. The intent's relevance window
//     is the vendor's idempotency window, so it expires rather than outliving
//     the guarantee it depends on.
const TypeAccountUpdaterBatchSubmit = "bt_account_updater_batch"

// AccountUpdaterIdempotencyWindow is how long BT caches an idempotency-key
// result. The intent expires with it: past this point a retry could mint a
// SECOND paid job, so the batch is abandoned (and its instruments become due
// again) rather than duplicated.
const AccountUpdaterIdempotencyWindow = 24 * time.Hour

// AccountUpdaterBatchIdempotencyKey is the logical identity of "submit THIS
// batch". The batch row is the unit of work, and the DB allows only one open
// batch per custodian, so the key needs nothing else.
func AccountUpdaterBatchIdempotencyKey(batchID uuid.UUID) string {
	return TypeAccountUpdaterBatchSubmit + ":" + batchID.String()
}

// AccountUpdaterBatchPayload names the durable batch and the custodian that
// owns it. The membership itself lives on the batch row — the payload stays
// small and the row stays the one place a resumed pass reads.
type AccountUpdaterBatchPayload struct {
	BatchID uuid.UUID `json:"batch_id"`
	// CustodianKind + CustodianAccountID are the custodian's vendor identity,
	// the only handle railresolve needs to arm its client.
	CustodianKind      string `json:"custodian_kind"`
	CustodianAccountID string `json:"custodian_account_id"`
}

// AccountUpdaterBatchInstrument is one member of the batch, recorded verbatim
// as it will be uploaded.
type AccountUpdaterBatchInstrument struct {
	PaymentMethodID uuid.UUID `json:"payment_method_id"`
	Token           string    `json:"token"`
	ExpirationMonth string    `json:"expiration_month,omitempty"`
	ExpirationYear  string    `json:"expiration_year,omitempty"`
}

// AccountUpdaterBatchHandler submits one assembled batch.
type AccountUpdaterBatchHandler struct {
	DB     *db.DB
	Config *config.Config
	Rails  railresolve.Source
	Clock  clockwork.Clock
	Policy BackoffPolicy
	// Outbound governs the PROVIDER-SUPPLIED upload url. Zero value is the
	// strict production policy; tests pass httpx.Policy{Allow: httpx.AllowLoopback}.
	Outbound httpx.Policy
	// BTBaseURLOverride points the client at a fake wire server in tests.
	BTBaseURLOverride string
}

func NewAccountUpdaterBatchHandler(d *db.DB, cfg *config.Config, rails railresolve.Source, clock clockwork.Clock) *AccountUpdaterBatchHandler {
	return &AccountUpdaterBatchHandler{DB: d, Config: cfg, Rails: rails, Clock: clock, Policy: DefaultBackoff}
}

func (h *AccountUpdaterBatchHandler) Type() string { return TypeAccountUpdaterBatchSubmit }

func (h *AccountUpdaterBatchHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}

func (h *AccountUpdaterBatchHandler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func decodeAccountUpdaterBatchPayload(intent gen.OpenrailsRailIntent) (AccountUpdaterBatchPayload, error) {
	var p AccountUpdaterBatchPayload
	if len(intent.Payload) == 0 {
		return p, errors.New("account updater batch intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode account updater batch payload: %w", err)
	}
	if p.BatchID == uuid.Nil || strings.TrimSpace(p.CustodianAccountID) == "" {
		return p, errors.New("account updater batch payload is incomplete")
	}
	return p, nil
}

// CheckRelevance: the submit applies while its batch is still pending. A batch
// already submitted, completed or abandoned has moved past this intent.
func (h *AccountUpdaterBatchHandler) CheckRelevance(ctx context.Context, intent gen.OpenrailsRailIntent) (Relevance, error) {
	p, err := decodeAccountUpdaterBatchPayload(intent)
	if err != nil {
		return StillRelevant(), nil // Execute reports the terminal payload error
	}
	batch, err := h.loadBatch(ctx, p.BatchID)
	if err != nil {
		if db.IsNotFound(err) {
			return SupersededBy("account updater batch row no longer exists"), nil
		}
		return Relevance{}, err
	}
	if batch.Status != "pending" {
		return SupersededBy("account updater batch is " + batch.Status + "; the submit is done or abandoned"), nil
	}
	return StillRelevant(), nil
}

func (h *AccountUpdaterBatchHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeAccountUpdaterBatchPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	batch, err := h.loadBatch(ctx, p.BatchID)
	if err != nil {
		if db.IsNotFound(err) {
			return Terminal("account updater batch row is gone; nothing to submit")
		}
		return Retryable("load account updater batch: " + err.Error())
	}
	if batch.Status != "pending" {
		// Already submitted (this attempt is a repeat) — success, no writes.
		return Succeeded(map[string]any{"already": batch.Status, "job_ref": batch.JobRef})
	}
	var instruments []AccountUpdaterBatchInstrument
	if err := json.Unmarshal(batch.Instruments, &instruments); err != nil {
		return Terminal("account updater batch membership is unreadable: " + err.Error())
	}
	if len(instruments) == 0 {
		return Terminal("account updater batch carries no instruments")
	}

	client, outcome, ok := h.client(ctx, p)
	if !ok {
		return outcome
	}
	if client.ReadOnly() {
		return Parked("basistheory client is read-only (mode=readonly)")
	}

	jobRef := strings.TrimSpace(batch.JobRef)
	var uploadURL string
	if jobRef == "" {
		// The BT-IDEMPOTENCY-KEY is the intent's own key: a create whose
		// response was lost returns the SAME job, never a second paid one.
		job, err := client.CreateAccountUpdaterJob(ctx, intent.IdempotencyKey)
		if err != nil {
			return Retryable("create account updater job: " + err.Error())
		}
		jobRef = strings.TrimSpace(job.ID)
		if jobRef == "" {
			return Retryable("custodian returned an account updater job with no id")
		}
		uploadURL = job.UploadURL
		// Record the job ref BEFORE uploading: a crash here must resume on this
		// job, not create another.
		if err := h.setJobRef(ctx, p.BatchID, jobRef); err != nil {
			return Retryable("persist account updater job ref: " + err.Error())
		}
	} else {
		// Resumed attempt: the upload url expires in an hour, so re-read the
		// job for a fresh one rather than trusting a stored one.
		job, err := client.GetAccountUpdaterJob(ctx, jobRef)
		if err != nil {
			return Retryable("re-read account updater job " + jobRef + ": " + err.Error())
		}
		uploadURL = job.UploadURL
	}
	if strings.TrimSpace(uploadURL) == "" {
		return Retryable("account updater job " + jobRef + " exposes no upload url yet")
	}

	rows := make([]basistheory.AccountUpdaterRequestRow, 0, len(instruments))
	for _, in := range instruments {
		rows = append(rows, basistheory.AccountUpdaterRequestRow{
			Token:           in.Token,
			ExpirationMonth: in.ExpirationMonth,
			ExpirationYear:  in.ExpirationYear,
		})
	}
	// Re-uploading the same CSV to the same job is the same object written
	// twice with the same bytes — safe by construction, which is what lets an
	// interrupted attempt simply run again.
	if err := client.UploadAccountUpdaterCSV(ctx, uploadURL, rows); err != nil {
		if errors.Is(err, basistheory.ErrProviderReadOnly) {
			return Parked("basistheory writes blocked (mode=readonly)")
		}
		return Retryable("upload account updater csv: " + err.Error())
	}

	// One transaction: the batch becomes the custodian's problem and the
	// instruments carry their watermark. A card is "checked" exactly when the
	// network took it.
	if err := h.markSubmitted(ctx, p.BatchID, instruments); err != nil {
		// The provider HAS the batch; the ingest pass polls the recorded job
		// even if this stamp is retried.
		return Retryable("mark account updater batch submitted: " + err.Error())
	}
	return Succeeded(map[string]any{
		"job_ref":     jobRef,
		"instruments": len(instruments),
	})
}

// Verify exists because the interface demands it. There is nothing ambiguous
// to resolve: the create is idempotent per intent key and the upload is a
// repeatable write of identical bytes, so every failure above is Retryable and
// this never runs in practice.
func (h *AccountUpdaterBatchHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) Outcome {
	p, err := decodeAccountUpdaterBatchPayload(intent)
	if err != nil {
		return Terminal(err.Error())
	}
	batch, err := h.loadBatch(ctx, p.BatchID)
	if err != nil {
		if db.IsNotFound(err) {
			return Terminal("account updater batch row is gone; nothing to verify")
		}
		return Ambiguous("load account updater batch: " + err.Error())
	}
	if batch.Status != "pending" {
		return Succeeded(map[string]any{"already": batch.Status, "job_ref": batch.JobRef})
	}
	return Retryable("account updater batch still pending; the submit is idempotent and re-runs")
}

func (h *AccountUpdaterBatchHandler) loadBatch(ctx context.Context, id uuid.UUID) (gen.OpenrailsAccountUpdaterBatch, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.OpenrailsAccountUpdaterBatch{}, err
	}
	return h.DB.Gen(ctx).GetAccountUpdaterBatch(ctx, gen.GetAccountUpdaterBatchParams{
		MerchantID: mid.UUID(), ID: id,
	})
}

func (h *AccountUpdaterBatchHandler) setJobRef(ctx context.Context, id uuid.UUID, jobRef string) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = h.DB.Gen(ctx).SetAccountUpdaterBatchJobRef(ctx, gen.SetAccountUpdaterBatchJobRefParams{
		MerchantID: mid.UUID(), ID: id, JobRef: jobRef,
	})
	return err
}

func (h *AccountUpdaterBatchHandler) markSubmitted(ctx context.Context, id uuid.UUID, instruments []AccountUpdaterBatchInstrument) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, 0, len(instruments))
	for _, in := range instruments {
		if in.PaymentMethodID != uuid.Nil {
			ids = append(ids, in.PaymentMethodID)
		}
	}
	now := h.now()
	return h.DB.MerchantTx(ctx, func(tctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.MarkAccountUpdaterBatchSubmitted(tctx, gen.MarkAccountUpdaterBatchSubmittedParams{
			MerchantID: mid.UUID(), ID: id, SubmittedAt: now,
		}); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		_, err := q.StampAccountUpdaterChecked(tctx, gen.StampAccountUpdaterCheckedParams{
			MerchantID: mid.UUID(), Ids: ids, CheckedAt: now,
		})
		return err
	})
}

// client arms the merchant's custodian client from the CUSTODIAN (or#880) —
// not from a PSP, since one custodian may back several and an account-updater
// job is about the instruments, not a gateway.
func (h *AccountUpdaterBatchHandler) client(ctx context.Context, p AccountUpdaterBatchPayload) (*basistheory.Client, Outcome, bool) {
	if h.Rails == nil {
		return nil, Parked("custodian resolution is not configured"), false
	}
	kind := strings.TrimSpace(p.CustodianKind)
	if kind == "" {
		kind = models.CustodianBasisTheory
	}
	cc, err := h.Rails.CustodianConfig(ctx, kind, p.CustodianAccountID)
	if err != nil {
		return nil, Parked(fmt.Sprintf("custodian %s/%s is not armed: %v", kind, p.CustodianAccountID, err)), false
	}
	if cc == nil {
		return nil, Parked(fmt.Sprintf("custodian %s/%s resolved to nothing", kind, p.CustodianAccountID)), false
	}
	if !cc.AccountUpdater {
		// The add-on was disarmed while a batch was open. Not our call to make
		// on the merchant's behalf, and calling anyway is a 403.
		return nil, Parked("custodian " + cc.Key + " no longer arms the account updater add-on"), false
	}
	baseURL := h.BTBaseURLOverride
	if baseURL == "" {
		baseURL = cc.APIBaseURL
	}
	client, err := basistheory.New(basistheory.Config{
		APIKey:        cc.APIKey,
		BaseURL:       baseURL,
		WebhookKeyURL: cc.WebhookKeyURL,
		ReadOnly:      h.Config != nil && h.Config.IsProviderReadOnly(),
		Outbound:      h.Outbound,
	})
	if err != nil {
		return nil, Parked("build custodian client: " + err.Error()), false
	}
	return client, Outcome{}, true
}
