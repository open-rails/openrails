//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// or#795 — the batch account updater end to end, against a fake Basis Theory
// wire server and a real Postgres.
//
// What these tests are the wall for:
//
//   - work scales with ACTIVITY. The pass must visit exactly the merchants
//     whose ARMED custodian holds a card renewing inside the window, and batch
//     exactly those cards. A worker that walked every payment method would
//     produce the same batch contents for the due merchant and be wrong.
//   - a batch in flight survives a restart. Killing the worker between submit
//     and results must resume by POLLING the recorded job, never by paying for
//     a second one.
//   - every result-code outcome lands through the EXISTING writers: UPD_*
//     rotates and CLEARS THE PARK (or#872), closed/contact-cardholder park,
//     no-match does nothing. Nothing is ever deleted or cancelled.
//   - RLS: one merchant can never see another's batches.

// --- fake Basis Theory ------------------------------------------------------

type fakeBT struct {
	srv *httptest.Server

	mu          sync.Mutex
	createCalls int
	// idempotent: one job per BT-IDEMPOTENCY-KEY, like the real API.
	byKey      map[string]string
	uploads    map[string]string // job id -> uploaded CSV
	results    map[string]string // job id -> result CSV (absent = still working)
	pollCalls  int
	jobCounter int
}

func newFakeBT(t *testing.T) *fakeBT {
	t.Helper()
	f := &fakeBT{
		byKey:   map[string]string{},
		uploads: map[string]string{},
		results: map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/account-updater/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.createCalls++
		key := r.Header.Get("BT-IDEMPOTENCY-KEY")
		id, ok := f.byKey[key]
		if !ok || key == "" {
			f.jobCounter++
			id = fmt.Sprintf("job_%d_%s", f.jobCounter, uuid.NewString()[:6])
			if key != "" {
				f.byKey[key] = id
			}
		}
		writeJSON(w, map[string]any{
			"id": id, "state": "pending", "upload_url": f.srv.URL + "/au-upload/" + id,
		})
	})
	mux.HandleFunc("/account-updater/jobs/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/account-updater/jobs/")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pollCalls++
		body := map[string]any{"id": id, "state": "processing", "upload_url": f.srv.URL + "/au-upload/" + id}
		if _, ok := f.results[id]; ok {
			body["state"] = "completed"
			body["download_url"] = f.srv.URL + "/au-download/" + id
		}
		writeJSON(w, body)
	})
	mux.HandleFunc("/au-upload/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/au-upload/")
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		f.mu.Lock()
		f.uploads[id] = string(body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/au-download/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/au-download/")
		f.mu.Lock()
		csv := f.results[id]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(csv))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeBT) creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

func (f *fakeBT) polls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pollCalls
}

func (f *fakeBT) uploadedTokens(t *testing.T, jobID string) []string {
	t.Helper()
	f.mu.Lock()
	csv := f.uploads[jobID]
	f.mu.Unlock()
	var tokens []string
	for i, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 { // header
			continue
		}
		tokens = append(tokens, strings.Split(line, ",")[0])
	}
	return tokens
}

func (f *fakeBT) publishResults(jobID, csv string) {
	f.mu.Lock()
	f.results[jobID] = csv
	f.mu.Unlock()
}

// --- fixture ----------------------------------------------------------------

type auMerchant struct {
	pspID    uuid.UUID
	id       uuid.UUID
	tenantID string
	pool     *pgxpool.Pool
	custID   uuid.UUID
}

type auFixture struct {
	t      *testing.T
	ctx    context.Context
	bt     *fakeBT
	dbi    *db.DB
	super  *pgxpool.Pool
	rails  railresolve.FixedSet
	cfg    *config.Config
	worker AccountUpdaterBatchWorker
}

func newAUFixture(t *testing.T) *auFixture {
	t.Helper()
	ctx := context.Background()
	fx := &auFixture{
		t:     t,
		ctx:   ctx,
		bt:    newFakeBT(t),
		dbi:   dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)), // UNPINNED, like a real River job
		super: dbtest.SharedSuperuserPGXPool(t),
		rails: railresolve.FixedSet{},
		cfg:   &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
	}
	// The cursor is deployment-global; other packages' fixtures must not decide
	// where this pass starts.
	_, err := fx.super.Exec(ctx, "DELETE FROM openrails.worker_sweep_cursors WHERE worker_kind = $1", KindAccountUpdaterBatch)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = fx.super.Exec(context.Background(),
			"DELETE FROM openrails.worker_sweep_cursors WHERE worker_kind = $1", KindAccountUpdaterBatch)
	})
	return fx
}

// build assembles the worker AFTER every merchant is seeded (the FixedSet
// resolver is populated by seedMerchant).
func (fx *auFixture) build() {
	loopback := httpx.Policy{Allow: httpx.AllowLoopback}
	handler := &intents.AccountUpdaterBatchHandler{
		DB:       fx.dbi,
		Config:   fx.cfg,
		Rails:    fx.rails,
		Policy:   intents.DefaultBackoff,
		Outbound: loopback,
	}
	fx.worker = AccountUpdaterBatchWorker{
		DB:     fx.dbi,
		Config: fx.cfg,
		Rails:  fx.rails,
		Intents: &intents.Runner{
			Store:    intents.NewStore(fx.dbi),
			Registry: intents.NewRegistry(handler),
			Config:   fx.cfg,
		},
		Outbound: loopback,
	}
}

// seedMerchant creates a merchant, optionally with an armed Basis Theory
// custodian. armed=false with declareCustodian=true is the "declared but the
// add-on was never bought" case; declareCustodian=false is a merchant that has
// no business being enumerated at all.
func (fx *auFixture) seedMerchant(declareCustodian, armed bool, lookaheadDays int) *auMerchant {
	t := fx.t
	t.Helper()
	m := &auMerchant{id: uuid.New(), tenantID: "tnt_" + uuid.NewString()[:8]}
	m.pool = dbtest.SharedMerchantPool(t, m.id)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := fx.super.Exec(fx.ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		m.id, "or795-"+m.id.String()[:8])

	if declareCustodian {
		m.custID = uuid.New()
		settings := map[string]any{"public_api_key": "key_pub_test"}
		if armed {
			settings[custodians.SettingAccountUpdater] = true
			if lookaheadDays > 0 {
				settings[custodians.SettingAccountUpdaterLookaheadDays] = lookaheadDays
			}
		}
		raw, err := json.Marshal(settings)
		require.NoError(t, err)
		exec(`INSERT INTO openrails.custodians (id, merchant_id, key, kind, environment, account_id, settings)
		      VALUES ($1, $2, $3, 'basis_theory', 'live', $4, $5)`,
			m.custID, m.id, "bt-"+m.id.String()[:8], m.tenantID, raw)

		m.pspID = uuid.New()
		exec(`INSERT INTO openrails.psps (id, merchant_id, rail, environment, account_id, custodian_id)
		      VALUES ($1, $2, 'nmi', 'live', $3, $4)`,
			m.pspID, m.id, "gw-"+m.id.String()[:6], m.custID)

		days := lookaheadDays
		if days <= 0 {
			days = custodians.DefaultAccountUpdaterLookaheadDays
		}
		fx.rails["nmi-"+m.id.String()[:8]] = &config.PSPConfig{
			Rail:      models.RailNMI,
			AccountID: "gw-" + m.id.String()[:6],
			NMI:       &config.NMIRailConfig{SecurityKey: "sk_test"},
			Custody: &config.CustodianConfig{
				Key:                         "bt-" + m.id.String()[:8],
				Custodian:                   models.CustodianBasisTheory,
				AccountID:                   m.tenantID,
				APIKey:                      "key_private_" + m.tenantID,
				APIBaseURL:                  fx.bt.srv.URL,
				AccountUpdater:              armed,
				AccountUpdaterLookaheadDays: days,
			},
		}
	}

	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range []string{
			"DELETE FROM openrails.rail_intents WHERE merchant_id = $1",
			"DELETE FROM openrails.account_updater_batches WHERE merchant_id = $1",
			"DELETE FROM openrails.subscriptions WHERE merchant_id = $1",
			"DELETE FROM openrails.payment_methods WHERE merchant_id = $1",
			"DELETE FROM openrails.psps WHERE merchant_id = $1",
			"DELETE FROM openrails.custodians WHERE merchant_id = $1",
			"DELETE FROM openrails.products WHERE merchant_id = $1",
			"DELETE FROM openrails.customers WHERE merchant_id = $1",
			"DELETE FROM openrails.merchants WHERE id = $1",
		} {
			_, _ = fx.super.Exec(bg, stmt, m.id)
		}
	})
	return m
}

type instrumentOpts struct {
	// renewsIn is how far ahead the backing subscription renews. Zero = no
	// subscription at all.
	renewsIn time.Duration
	// status of the backing subscription (default active).
	status string
	// checkedAgo: how long ago the account updater last saw this card. Zero =
	// never checked.
	checkedAgo time.Duration
	parkReason string
	expiry     string
}

// seedInstrument creates one custodian-held card — with its own customer, so
// the one-live-subscription-per-(customer, product) invariant is not what this
// fixture is testing — optionally backed by a subscription. Returns the
// payment-method id and its custodian token.
func (fx *auFixture) seedInstrument(m *auMerchant, opts instrumentOpts) (uuid.UUID, string) {
	t := fx.t
	t.Helper()
	customer, product := uuid.New(), uuid.New()
	_, err := fx.super.Exec(fx.ctx,
		`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
		customer, m.id, uuid.NewString())
	require.NoError(t, err)
	_, err = fx.super.Exec(fx.ctx,
		`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		product, "or795-prod-"+uuid.NewString()[:8], m.id)
	require.NoError(t, err)

	pmID := uuid.New()
	token := "tok_" + uuid.NewString()
	expiry := opts.expiry
	if expiry == "" {
		expiry = "12/31"
	}
	var checked *time.Time
	if opts.checkedAgo > 0 {
		at := time.Now().UTC().Add(-opts.checkedAgo)
		checked = &at
	}
	var parkedAt *time.Time
	if opts.parkReason != "" {
		at := time.Now().UTC()
		parkedAt = &at
	}
	_, err = fx.super.Exec(fx.ctx, `
		INSERT INTO openrails.payment_methods
		  (id, rail, initial_transaction_id, merchant_id, customer_id, custodian,
		   rail_method_ref, expiry_date, last_four, card_type, fingerprint,
		   account_updater_checked_at, park_reason, parked_at, psp_id)
		VALUES ($1, 'nmi', '', $2, $3, 'basis_theory', $4, $5, '1111', 'visa', $6, $7, $8, $9, $10)`,
		pmID, m.id, customer, token, expiry, "fp_"+uuid.NewString()[:8], checked, opts.parkReason, parkedAt, m.pspID)
	require.NoError(t, err)

	if opts.renewsIn != 0 {
		status := opts.status
		if status == "" {
			status = "active"
		}
		now := time.Now().UTC()
		subID := uuid.New()
		var cancelledAt *time.Time
		var cancelType *string
		if status == "cancelled" {
			at := now.Add(-time.Hour)
			ct := "immediate"
			cancelledAt, cancelType = &at, &ct
		}
		_, err := fx.super.Exec(fx.ctx, `
			INSERT INTO openrails.subscriptions
			  (id, product_id, status, rail, current_period_starts_at, current_period_ends_at,
			   started_at, payment_method_id, customer_id, merchant_id, cancelled_at, cancel_type, psp_id)
			VALUES ($1, $2, $3, 'nmi', $4, $5, $4, $6, $7, $8, $9, $10, $11)`,
			subID, product, status, now.Add(-24*time.Hour), now.Add(opts.renewsIn),
			pmID, customer, m.id, cancelledAt, cancelType, m.pspID)
		require.NoError(t, err)
	}
	return pmID, token
}

func (fx *auFixture) methodRow(m *auMerchant, id uuid.UUID) map[string]any {
	fx.t.Helper()
	row := map[string]any{}
	var (
		methodRef, parkReason, cardType, fingerprint string
		expiry                                       *string
		lastFour                                     *string
		checkedAt, parkedAt                          *time.Time
	)
	require.NoError(fx.t, m.pool.QueryRow(fx.ctx, `
		SELECT rail_method_ref, park_reason, COALESCE(card_type,''), fingerprint, expiry_date,
		       last_four, account_updater_checked_at, parked_at
		  FROM openrails.payment_methods WHERE id = $1`, id).
		Scan(&methodRef, &parkReason, &cardType, &fingerprint, &expiry, &lastFour, &checkedAt, &parkedAt))
	row["rail_method_ref"] = methodRef
	row["park_reason"] = parkReason
	row["card_type"] = cardType
	row["fingerprint"] = fingerprint
	row["expiry_date"] = expiry
	row["last_four"] = lastFour
	row["checked_at"] = checkedAt
	row["parked_at"] = parkedAt
	return row
}

type batchRow struct {
	ID           uuid.UUID
	JobRef       string
	Status       string
	ResultCounts map[string]int
	SubmittedAt  *time.Time
	LastPolledAt *time.Time
	CompletedAt  *time.Time
	Failure      string
}

func (fx *auFixture) batches(m *auMerchant) []batchRow {
	fx.t.Helper()
	rows, err := m.pool.Query(fx.ctx, `
		SELECT id, job_ref, status, result_counts, submitted_at, last_polled_at, completed_at, failure_reason
		  FROM openrails.account_updater_batches WHERE merchant_id = $1 ORDER BY created_at`, m.id)
	require.NoError(fx.t, err)
	defer rows.Close()
	var out []batchRow
	for rows.Next() {
		var b batchRow
		var counts []byte
		require.NoError(fx.t, rows.Scan(&b.ID, &b.JobRef, &b.Status, &counts, &b.SubmittedAt, &b.LastPolledAt, &b.CompletedAt, &b.Failure))
		require.NoError(fx.t, json.Unmarshal(counts, &b.ResultCounts))
		out = append(out, b)
	}
	require.NoError(fx.t, rows.Err())
	return out
}

func containsID(ids []uuid.UUID, want uuid.UUID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- tests ------------------------------------------------------------------

// The activity wall: exactly the cards renewing inside the window, on ARMED
// custodians, that have not been refreshed this cycle — and nothing else.
func TestAccountUpdaterBatchesExactlyTheDueInstruments(t *testing.T) {
	fx := newAUFixture(t)
	armed := fx.seedMerchant(true, true, 14)
	other := fx.seedMerchant(true, true, 14)   // a second armed merchant: its own batch
	unarmed := fx.seedMerchant(true, false, 0) // declared the custodian, never bought the add-on
	noCustodian := fx.seedMerchant(false, false, 0)
	fx.build()

	dueNever, tokenNever := fx.seedInstrument(armed, instrumentOpts{renewsIn: 5 * 24 * time.Hour})
	dueStale, tokenStale := fx.seedInstrument(armed, instrumentOpts{
		renewsIn: 13 * 24 * time.Hour, checkedAgo: 30 * 24 * time.Hour})
	// A parked card IS due: the updater is what recovers it (or#872).
	dueParked, tokenParked := fx.seedInstrument(armed, instrumentOpts{
		renewsIn: 3 * 24 * time.Hour, parkReason: "bt_token_expired"})
	farOff, _ := fx.seedInstrument(armed, instrumentOpts{renewsIn: 40 * 24 * time.Hour})
	fresh, _ := fx.seedInstrument(armed, instrumentOpts{
		renewsIn: 5 * 24 * time.Hour, checkedAgo: 2 * 24 * time.Hour})
	noSub, _ := fx.seedInstrument(armed, instrumentOpts{})
	cancelled, _ := fx.seedInstrument(armed, instrumentOpts{
		renewsIn: 5 * 24 * time.Hour, status: "cancelled"})

	_, otherToken := fx.seedInstrument(other, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	fx.seedInstrument(unarmed, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	fx.seedInstrument(noCustodian, instrumentOpts{renewsIn: 2 * 24 * time.Hour})

	result, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)

	require.True(t, containsID(result.SubmitMerchants, armed.id), "the armed merchant with due cards must be visited")
	require.True(t, containsID(result.SubmitMerchants, other.id), "each armed merchant gets its own batch")
	require.False(t, containsID(result.SubmitMerchants, unarmed.id),
		"a custodian without the account-updater add-on is not work: calling anyway is a 403, not a refresh")
	require.False(t, containsID(result.SubmitMerchants, noCustodian.id),
		"a merchant with no custodian must never be enumerated")

	// One batch per armed merchant, each submitted with a job ref.
	armedBatches := fx.batches(armed)
	require.Len(t, armedBatches, 1)
	require.Equal(t, "submitted", armedBatches[0].Status)
	require.NotEmpty(t, armedBatches[0].JobRef)
	require.NotNil(t, armedBatches[0].SubmittedAt)
	require.Len(t, fx.batches(other), 1)
	require.Empty(t, fx.batches(unarmed))
	require.Empty(t, fx.batches(noCustodian))

	// The CSV that actually reached the wire carries exactly the due tokens.
	uploaded := fx.bt.uploadedTokens(t, armedBatches[0].JobRef)
	require.ElementsMatch(t, []string{tokenNever, tokenStale, tokenParked}, uploaded,
		"the batch must carry every due card and no other")
	require.NotContains(t, uploaded, otherToken, "one merchant's cards never ride another's batch")

	// The watermark moved for exactly those cards.
	for _, id := range []uuid.UUID{dueNever, dueStale, dueParked} {
		require.NotNil(t, fx.methodRow(armed, id)["checked_at"], "a batched card carries its watermark")
	}
	for _, id := range []uuid.UUID{farOff, noSub, cancelled} {
		require.Nil(t, fx.methodRow(armed, id)["checked_at"], "an undue card must not be stamped")
	}
	freshRow := fx.methodRow(armed, fresh)
	require.NotNil(t, freshRow["checked_at"])
	require.WithinDuration(t, time.Now().UTC().Add(-2*24*time.Hour), *(freshRow["checked_at"].(*time.Time)), time.Minute,
		"a card refreshed inside the window keeps its ORIGINAL watermark; it was not re-submitted")

	// The park survives the submit: only a result may clear it (or#872).
	require.Equal(t, "bt_token_expired", fx.methodRow(armed, dueParked)["park_reason"])

	// A second pass while the batch is open must not open another.
	result2, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Zero(t, result2.BatchesCreated, "one open batch per custodian")
	require.Len(t, fx.batches(armed), 1)
}

// The restart wall: a worker killed between submit and results must RESUME by
// polling the recorded job. A resubmit is a second paid batch and a second set
// of network lookups for cards we already asked about.
func TestAccountUpdaterResumesPollingAfterRestartWithoutResubmitting(t *testing.T) {
	fx := newAUFixture(t)
	m := fx.seedMerchant(true, true, 14)
	fx.build()
	pmID, token := fx.seedInstrument(m, instrumentOpts{renewsIn: 4 * 24 * time.Hour})

	// Pass 1: submit.
	_, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, fx.bt.creates())
	batch := fx.batches(m)[0]
	require.Equal(t, "submitted", batch.Status)
	jobRef := batch.JobRef

	// --- the crash. Every later pass runs on a FRESH worker over a bare
	// context: nothing but the durable row carries the job forward.
	fx.build()

	// Pass 2: the custodian is still working. Poll, record it, submit nothing.
	_, err = fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, fx.bt.creates(), "a restart must never mint a second job")
	after := fx.batches(m)
	require.Len(t, after, 1)
	require.Equal(t, "submitted", after[0].Status)
	require.Equal(t, jobRef, after[0].JobRef, "the SAME job is resumed")
	require.NotNil(t, after[0].LastPolledAt, "the poll must be recorded")
	require.GreaterOrEqual(t, fx.bt.polls(), 1, "resuming means POLLING the recorded job")

	// Pass 3: results land, on yet another fresh worker.
	fx.bt.publishResults(jobRef, auResultCSV(
		auResult{token: token, code: "UPD_EXP_DATE", expMonth: "9", expYear: "2034", last4: "1111"}))
	fx.build()
	result, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, fx.bt.creates(), "still one job, ever")
	require.Equal(t, 1, result.BatchesCompleted)

	done := fx.batches(m)[0]
	require.Equal(t, "completed", done.Status)
	require.NotNil(t, done.CompletedAt)
	require.Equal(t, map[string]int{"UPD_EXP_DATE": 1}, done.ResultCounts,
		"the result vocabulary is recorded verbatim")

	// The refresh went through the rotate path.
	row := fx.methodRow(m, pmID)
	require.Equal(t, "09/34", *(row["expiry_date"].(*string)))
	require.Equal(t, token, row["rail_method_ref"], "an in-place update keeps the token")
}

// Every result outcome, through the ONE set of writers. Nothing is deleted,
// nothing is cancelled: recovery rotates and un-parks, refusal parks.
func TestAccountUpdaterFoldsTheResultVocabularyThroughTheRotatePath(t *testing.T) {
	fx := newAUFixture(t)
	m := fx.seedMerchant(true, true, 14)
	fx.build()

	rotated, tokRotated := fx.seedInstrument(m, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	// Parked by an earlier token.expired: the updater's answer must free it.
	recovered, tokRecovered := fx.seedInstrument(m, instrumentOpts{
		renewsIn: 2 * 24 * time.Hour, parkReason: "bt_token_expired", expiry: "01/24"})
	closed, tokClosed := fx.seedInstrument(m, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	contact, tokContact := fx.seedInstrument(m, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	untouched, tokUntouched := fx.seedInstrument(m, instrumentOpts{renewsIn: 2 * 24 * time.Hour})
	unknown, tokUnknown := fx.seedInstrument(m, instrumentOpts{renewsIn: 2 * 24 * time.Hour})

	_, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	jobRef := fx.batches(m)[0].JobRef

	newToken := "tok_" + uuid.NewString()
	fx.bt.publishResults(jobRef, auResultCSV(
		auResult{token: tokRotated, code: "UPD_PAN", newToken: newToken, expMonth: "7", expYear: "2033",
			fingerprint: "fp_rotated", brand: "mastercard", last4: "4444"},
		auResult{token: tokRecovered, code: "UPD_EXP_DATE", expMonth: "11", expYear: "2035", last4: "1111"},
		auResult{token: tokClosed, code: "WRN_CLOSED_ACCOUNT"},
		auResult{token: tokContact, code: "WRN_CONTACT_CARDHOLDER"},
		auResult{token: tokUntouched, code: "NO_MATCH"},
		auResult{token: tokUnknown, code: "SOMETHING_NEW"},
	))
	fx.build()
	result, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, result.BatchesCompleted)

	// UPD_PAN: the ref rotates and every masked field is refreshed.
	got := fx.methodRow(m, rotated)
	require.Equal(t, newToken, got["rail_method_ref"])
	require.Equal(t, "fp_rotated", got["fingerprint"])
	require.Equal(t, "mastercard", got["card_type"])
	require.Equal(t, "4444", *(got["last_four"].(*string)))
	require.Equal(t, "07/33", *(got["expiry_date"].(*string)))

	// or#872: an UPD_* row is the newest evidence about the card, so the park
	// the expiry event set must NOT outlive it.
	got = fx.methodRow(m, recovered)
	require.Equal(t, "11/35", *(got["expiry_date"].(*string)))
	require.Empty(t, got["park_reason"], "the updater recovered the card; the park must be cleared")
	require.Nil(t, got["parked_at"])
	require.Equal(t, tokRecovered, got["rail_method_ref"])

	// Closed / contact-cardholder park (bucket 2) — never cancelled, never gone.
	require.Equal(t, "bt_au_closed_account", fx.methodRow(m, closed)["park_reason"])
	require.Equal(t, tokClosed, fx.methodRow(m, closed)["rail_method_ref"], "the row still exists")
	require.Equal(t, "bt_au_contact_cardholder", fx.methodRow(m, contact)["park_reason"])

	// NO_MATCH and an unknown code are recorded and folded by nothing.
	require.Empty(t, fx.methodRow(m, untouched)["park_reason"])
	require.Equal(t, tokUntouched, fx.methodRow(m, untouched)["rail_method_ref"])
	require.Empty(t, fx.methodRow(m, unknown)["park_reason"])
	require.Equal(t, tokUnknown, fx.methodRow(m, unknown)["rail_method_ref"])

	require.Equal(t, map[string]int{
		"UPD_PAN": 1, "UPD_EXP_DATE": 1, "WRN_CLOSED_ACCOUNT": 1,
		"WRN_CONTACT_CARDHOLDER": 1, "NO_MATCH": 1, "SOMETHING_NEW": 1,
	}, fx.batches(m)[0].ResultCounts, "every code, verbatim, including the one we do not understand")
}

// A capped pass must be bounded AND resumable: the cursor is what stops a cap
// from serving the same head forever (or#837).
func TestAccountUpdaterCappedPassResumesWhereItLeftOff(t *testing.T) {
	fx := newAUFixture(t)
	var seeded []uuid.UUID
	for i := 0; i < 4; i++ {
		m := fx.seedMerchant(true, true, 14)
		seeded = append(seeded, m.id)
		fx.build()
		fx.seedInstrument(m, instrumentOpts{renewsIn: 3 * 24 * time.Hour})
	}
	fx.build()
	fx.worker.MerchantBatch = 2

	first, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Len(t, first.SubmitMerchants, 2, "the pass must stop at the cap")

	var cursor *uuid.UUID
	require.NoError(t, fx.super.QueryRow(fx.ctx,
		"SELECT cursor_merchant_id FROM openrails.worker_sweep_cursors WHERE worker_kind = $1",
		KindAccountUpdaterBatch).Scan(&cursor))
	require.NotNil(t, cursor, "a capped pass must leave a resume point")
	require.Equal(t, first.SubmitMerchants[len(first.SubmitMerchants)-1], *cursor)

	seen := map[uuid.UUID]bool{}
	for _, id := range first.SubmitMerchants {
		seen[id] = true
	}
	for pass := 0; pass < 12; pass++ {
		next, err := fx.worker.RunPass(fx.ctx)
		require.NoError(t, err)
		for _, id := range next.SubmitMerchants {
			seen[id] = true
		}
	}
	for _, id := range seeded {
		require.True(t, seen[id], "merchant %s never got a pass — the cap starved the tail", id)
	}
}

// RLS: a merchant can never see another's batches. The batch row names the
// cards a merchant holds at its custodian.
func TestAccountUpdaterBatchesAreMerchantIsolated(t *testing.T) {
	fx := newAUFixture(t)
	a := fx.seedMerchant(true, true, 14)
	b := fx.seedMerchant(true, true, 14)
	fx.build()
	fx.seedInstrument(a, instrumentOpts{renewsIn: 3 * 24 * time.Hour})
	fx.seedInstrument(b, instrumentOpts{renewsIn: 3 * 24 * time.Hour})

	_, err := fx.worker.RunPass(fx.ctx)
	require.NoError(t, err)
	require.Len(t, fx.batches(a), 1)
	require.Len(t, fx.batches(b), 1)

	// b's RLS-enforcing handle cannot see a's row, by id or at all.
	var n int
	require.NoError(t, b.pool.QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.account_updater_batches WHERE id = $1", fx.batches(a)[0].ID).Scan(&n))
	require.Zero(t, n, "merchant B must not be able to read merchant A's batch")

	require.NoError(t, b.pool.QueryRow(fx.ctx,
		"SELECT count(*) FROM openrails.account_updater_batches WHERE merchant_id = $1", a.id).Scan(&n))
	require.Zero(t, n)
}

// --- result CSV helpers -----------------------------------------------------

type auResult struct {
	token       string
	code        string
	newToken    string
	expMonth    string
	expYear     string
	fingerprint string
	brand       string
	last4       string
}

func auResultCSV(rows ...auResult) string {
	var b strings.Builder
	b.WriteString("token,expiration_year,expiration_month,new_token,new_expiration_year," +
		"new_expiration_month,result_code,new_fingerprint,new_brand,new_last4\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s,,,%s,%s,%s,%s,%s,%s,%s\n",
			r.token, r.newToken, r.expYear, r.expMonth, r.code, r.fingerprint, r.brand, r.last4)
	}
	return b.String()
}
