//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/open-rails/openrails/internal/modules/money"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ============================================================================
// #692 operator findings queue — REAL Postgres + fake NMI HTTP server (the
// HTTP server is the only fake; DB, intents, executor, findings, converge all
// real).
// ============================================================================

// fakeNMIGateway scripts the v5 gateway: subscription reads/deletes (the
// deferred-delete intent) and refunds (the admin refund producer).
type fakeNMIGateway struct {
	srv          *httptest.Server
	refundStatus atomic.Value // string body override for POST .../refund ("" = success)
	refundCalls  atomic.Int64
	deleteCalls  atomic.Int64
	subDeleted   atomic.Bool
}

func newFakeNMIGateway(t *testing.T) (*fakeNMIGateway, *nmi.NMIClient) {
	t.Helper()
	f := &fakeNMIGateway{}
	f.refundStatus.Store("")
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/subscriptions/"):
			if f.subDeleted.Load() {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","message":"not found"}`)
				return
			}
			fmt.Fprint(w, `{"object":"subscription","id":"psid","delayed_condition":"active"}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/subscriptions/"):
			f.deleteCalls.Add(1)
			f.subDeleted.Store(true)
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund"):
			f.refundCalls.Add(1)
			if body := f.refundStatus.Load().(string); body != "" {
				fmt.Fprint(w, body)
				return
			}
			fmt.Fprint(w, `{"object":"transaction","id":"txn_refund_1","response":"1","response_text":"SUCCESS"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(f.srv.Close)

	client, err := nmi.NewClient("nmi", &config.NMIProviderSettings{
		SecurityKey:   "test_security_key",
		WebhookSecret: "test_secret",
	}, true)
	require.NoError(t, err)
	client.DirectPostURL = f.srv.URL
	client.QueryURL = f.srv.URL
	client.V5BaseURL = f.srv.URL
	return f, client
}

// findingsFixture is a DEDICATED merchant per test — queue/gauge assertions
// stay deterministic on the shared database.
type findingsFixture struct {
	t        *testing.T
	dbi      *db.DB
	ctx      context.Context
	merchant uuid.UUID
	customer uuid.UUID
	product  uuid.UUID
	price    uuid.UUID
	adminID  string
	fake     *fakeNMIGateway
	client   *nmi.NMIClient
	rt       *app.Runtime
}

func newFindingsFixture(t *testing.T) *findingsFixture {
	t.Helper()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	mid := uuid.New()
	ctx := merchant.WithID(context.Background(), merchant.ID(mid))
	sfx := uuid.NewString()[:8]

	fx := &findingsFixture{
		t: t, dbi: dbi, ctx: ctx, merchant: mid,
		adminID: "admin-" + sfx,
		product: uuid.New(), price: uuid.New(),
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, mid, "findings-"+sfx)
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		fx.product, "findings-prod-"+sfx, mid)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 10000000, 'usd', 720, true, $3)`, fx.price, fx.product, mid)
	custID, err := gen.New(pool).EnsureCustomer(ctx, gen.EnsureCustomerParams{ID: uuid.New(), MerchantID: mid, Subject: nil})
	require.NoError(t, err)
	fx.customer = custID

	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range []string{
			`DELETE FROM openrails.rail_mutation_logs WHERE merchant_id = $1`,
			`DELETE FROM openrails.rail_intents WHERE merchant_id = $1`,
			`DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`,
			`DELETE FROM openrails.reconciliation_runs WHERE merchant_id = $1`,
			`DELETE FROM openrails.notification_queue WHERE merchant_id = $1`,
			`DELETE FROM openrails.product_access_grants WHERE merchant_id = $1`,
			`DELETE FROM openrails.entitlements WHERE merchant_id = $1`,
			`DELETE FROM openrails.grants WHERE merchant_id = $1`,
			`DELETE FROM openrails.payments WHERE merchant_id = $1`,
			`DELETE FROM openrails.subscriptions WHERE merchant_id = $1`,
			`DELETE FROM openrails.prices WHERE merchant_id = $1`,
			`DELETE FROM openrails.products WHERE merchant_id = $1`,
			`DELETE FROM openrails.customers WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = pool.Exec(bg, stmt, mid)
		}
	})

	fx.fake, fx.client = newFakeNMIGateway(t)
	clock := clockwork.NewRealClock()
	priceSvc := catalog.NewPriceService(dbi)
	productSvc := catalog.NewProductService(dbi)
	entSvc := entitlements.NewEntitlementService(dbi, clock)
	paySvc := payments.NewPaymentService(dbi, clock)
	fx.rt = &app.Runtime{
		DB:                           dbi,
		Config:                       &config.Config{ProviderWriteMode: config.ProviderWriteModeFull},
		Clock:                        clock,
		CollectionResolver:           findingsNMIResolver{client: fx.client},
		SubscriptionService:          subscriptions.NewSubscriptionService(dbi, priceSvc, productSvc, nil, clock),
		SubscriptionLifecycleService: subscriptions.NewSubscriptionLifecycleService(dbi, productSvc, priceSvc, entSvc, nil, paySvc, clock),
		PaymentService:               paySvc,
		EntitlementService:           entSvc,
		ProductAccessService:         productaccess.NewService(dbi, clock),
	}
	return fx
}

// findingsNMIResolver is a fixed money.CollectionPlane (#788 test stand-in).
type findingsNMIResolver struct{ client *nmi.NMIClient }

func (f findingsNMIResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if f.client == nil {
		return nil, false, nil
	}
	return f.client, true, nil
}

func (f findingsNMIResolver) ResolveCollectionAdapter(context.Context, gen.OpenrailsPaymentMethod) (money.CollectionAdapter, bool, error) {
	return nil, false, nil
}

func (f findingsNMIResolver) VerifyCollectionCharge(context.Context, gen.OpenrailsPaymentMethod, string) (money.CollectionVerifyResult, error) {
	return money.CollectionVerifyResult{}, nil
}

func (fx *findingsFixture) exec(sql string, args ...any) {
	fx.t.Helper()
	_, err := fx.dbi.Pool().Exec(fx.ctx, sql, args...)
	require.NoError(fx.t, err)
}

func (fx *findingsFixture) seedActiveSubscription(psid string) uuid.UUID {
	return fx.seedActiveSubscriptionFor(fx.product, fx.price, psid)
}

func (fx *findingsFixture) seedActiveSubscriptionFor(productID, priceID uuid.UUID, psid string) uuid.UUID {
	fx.t.Helper()
	subID := uuid.New()
	now := time.Now().UTC()
	fx.exec(`INSERT INTO openrails.subscriptions
	          (id, price_id, product_id, status, rail, rail_subscription_id,
	           current_period_starts_at, current_period_ends_at, started_at, customer_id, merchant_id)
	        VALUES ($1, $2, $3, 'active', 'nmi', $4, $5, $6, $5, $7, $8)`,
		subID, priceID, productID, psid, now.Add(-24*time.Hour), now.Add(29*24*time.Hour), fx.customer, fx.merchant)
	return subID
}

// seedSecondProduct: one live subscription per (customer, product) is
// schema-blocked, so a second concurrent sub needs its own product.
func (fx *findingsFixture) seedSecondProduct() (productID, priceID uuid.UUID) {
	fx.t.Helper()
	productID, priceID = uuid.New(), uuid.New()
	sfx := uuid.NewString()[:8]
	fx.exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "findings-prod2-"+sfx, fx.merchant)
	fx.exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	         VALUES ($1, $2, 10000000, 'usd', 8760, true, $3)`, priceID, productID, fx.merchant)
	return productID, priceID
}

func (fx *findingsFixture) seedCompletedPayment(txn string, subID *uuid.UUID) uuid.UUID {
	fx.t.Helper()
	payID := uuid.New()
	fx.exec(`INSERT INTO openrails.payments
	          (id, price_id, rail, transaction_id, amount, list_amount, currency, status, subscription_id, customer_id, merchant_id)
	        VALUES ($1, $2, 'nmi', $3, 10000000, 10000000, 'usd', 'completed', $4, $5, $6)`,
		payID, fx.price, txn, subID, fx.customer, fx.merchant)
	return payID
}

func (fx *findingsFixture) seedFinding(ftype, subject, severity, prose string, rec *recommend.Recommendation) uuid.UUID {
	fx.t.Helper()
	evidence := map[string]any{"provider": "nmi"}
	if rec != nil {
		evidence[recommend.EvidenceKey] = rec.Map()
	}
	b, err := json.Marshal(evidence)
	require.NoError(fx.t, err)
	var prosePtr *string
	if prose != "" {
		prosePtr = &prose
	}
	row, err := gen.New(fx.dbi.Pool()).UpsertReconciliationFinding(fx.ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        fx.merchant,
		FindingType:       ftype,
		SubjectKey:        subject,
		Severity:          severity,
		Status:            "requires_review",
		RecommendedAction: prosePtr,
		Evidence:          b,
		RunID:             nil,
	})
	require.NoError(fx.t, err)
	return row.ID
}

func (fx *findingsFixture) do(handler func(*httprequest.Request), method, target string, body any, pathID string) *httptest.ResponseRecorder {
	fx.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(fx.t, err)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rd).WithContext(fx.ctx)
	if pathID != "" {
		req.SetPathValue("id", pathID)
	}
	rec := httptest.NewRecorder()
	hr := httprequest.NewHTTP(rec, req, fx.rt)
	hr.SetUserContext(billingauth.UserContext{UserID: fx.adminID})
	handler(hr)
	return rec
}

type findingItemBody struct {
	ID             string                    `json:"id"`
	FindingType    string                    `json:"finding_type"`
	Severity       string                    `json:"severity"`
	Status         string                    `json:"status"`
	Resolution     string                    `json:"resolution"`
	ResolvedBy     string                    `json:"resolved_by"`
	OperatorNotes  string                    `json:"operator_notes"`
	Recommendation *recommend.Recommendation `json:"recommendation"`
	Evidence       map[string]any            `json:"evidence"`
}

type findingsListBody struct {
	Items  []findingItemBody `json:"items"`
	Total  int64             `json:"total"`
	Gauges struct {
		Freeloaders       int64            `json:"freeloaders"`
		DuplicateCoverage int64            `json:"duplicate_coverage"`
		OpenBySeverity    map[string]int64 `json:"open_by_severity"`
		TotalOpen         int64            `json:"total_open"`
	} `json:"gauges"`
}

type resolveBody struct {
	Finding   findingItemBody `json:"finding"`
	Execution map[string]any  `json:"execution"`
}

func (fx *findingsFixture) list(query string) findingsListBody {
	fx.t.Helper()
	rec := fx.do(AdminListFindings, http.MethodGet, "/findings"+query, nil, "")
	require.Equal(fx.t, http.StatusOK, rec.Code, rec.Body.String())
	var body findingsListBody
	require.NoError(fx.t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body
}

func (fx *findingsFixture) findingRow(id uuid.UUID) gen.OpenrailsReconciliationFinding {
	fx.t.Helper()
	row, err := gen.New(fx.dbi.Pool()).GetReconciliationFinding(fx.ctx, id)
	require.NoError(fx.t, err)
	return row
}

func (fx *findingsFixture) deleteRunner() *intents.Runner {
	return &intents.Runner{
		Store:    intents.NewStore(fx.dbi),
		Registry: intents.NewRegistry(intents.NewNMIDeleteHandler(fx.dbi, fx.rt.Config, findingsNMIResolver{client: fx.client}, nil)),
		Config:   fx.rt.Config,
		Breaker:  intents.NewVolumeBreaker(fx.dbi),
	}
}

// TestFindingsQueueApproveCancelAndRefundEndToEnd: seed a duplicate-shaped
// finding with the structured recommendation (#690's detector lands later) →
// list shows it sorted/filterable with gauges → approve executes cancel (local
// terminal + durable delete intent) + refund (through the intents ledger
// against the fake gateway, recorded on the payments ledger) → finding fixed
// with resolved_by/notes → a later converge sweep does NOT reopen it.
func TestFindingsQueueApproveCancelAndRefundEndToEnd(t *testing.T) {
	fx := newFindingsFixture(t)
	psid := "psid-" + uuid.NewString()[:8]
	txn := "txn-" + uuid.NewString()[:8]
	subID := fx.seedActiveSubscription(psid)
	payID := fx.seedCompletedPayment(txn, &subID)

	// Older, lower-severity finding first: proves severity-desc sort and the
	// freeloader gauge type-set; it has NO structured recommendation.
	orphanID := fx.seedFinding("derive.entitlement.unjustified", "ent:"+uuid.NewString(), "high",
		"Live access with no recorded justification — revoke or record an admin grant.", nil)
	dupID := fx.seedFinding("consistency.duplicate.ownership",
		"customer:"+fx.customer.String()+":product:"+fx.product.String(), "critical",
		"Customer holds two overlapping subscriptions; cancel the later-created and refund the duplicate payment.",
		&recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{
				"subscription_id":   subID.String(),
				"refund_payment_id": payID.String(),
			},
		})

	// --- list: sort, gauges, filters ---
	body := fx.list("")
	require.Len(t, body.Items, 2)
	assert.Equal(t, dupID.String(), body.Items[0].ID, "critical sorts before high despite being newer")
	assert.Equal(t, orphanID.String(), body.Items[1].ID)
	assert.EqualValues(t, 2, body.Total)
	assert.EqualValues(t, 1, body.Gauges.DuplicateCoverage)
	assert.EqualValues(t, 1, body.Gauges.Freeloaders)
	assert.EqualValues(t, 2, body.Gauges.TotalOpen)
	assert.EqualValues(t, 1, body.Gauges.OpenBySeverity["critical"])
	require.NotNil(t, body.Items[0].Recommendation)
	assert.Equal(t, recommend.ActionCancelAndRefund, body.Items[0].Recommendation.Action)
	assert.Nil(t, body.Items[1].Recommendation, "prose-only finding has no mechanical fix")

	filtered := fx.list("?severity=critical")
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, dupID.String(), filtered.Items[0].ID)
	filtered = fx.list("?finding_type=derive.entitlement.unjustified")
	require.Len(t, filtered.Items, 1)
	assert.Equal(t, orphanID.String(), filtered.Items[0].ID)

	// --- GET one ---
	rec := fx.do(AdminGetFinding, http.MethodGet, "/findings/"+dupID.String(), nil, dupID.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var single findingItemBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &single))
	require.NotNil(t, single.Recommendation)
	assert.Equal(t, subID.String(), single.Recommendation.Params["subscription_id"])

	// --- approve ---
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+dupID.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "duplicate confirmed; refunding the later charge"}, dupID.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved resolveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, "cancelled", resolved.Execution["cancel"])
	assert.Equal(t, "queued", resolved.Execution["delete_intent"])
	assert.Equal(t, "completed", resolved.Execution["refund"])
	assert.Equal(t, "fixed", resolved.Finding.Status)
	assert.Equal(t, "admin_fixed", resolved.Finding.Resolution)
	assert.Equal(t, fx.adminID, resolved.Finding.ResolvedBy)
	assert.Contains(t, resolved.Finding.OperatorNotes, "duplicate confirmed")

	// Subscription cancelled locally; the remote delete is a DURABLE intent.
	var subStatus string
	var marker *time.Time
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status, deletion_scheduled_at FROM openrails.subscriptions WHERE id = $1`, subID).
		Scan(&subStatus, &marker))
	assert.Equal(t, "cancelled", subStatus)
	require.NotNil(t, marker, "deferred-delete marker persists until the intent executes")

	var deleteIntentID uuid.UUID
	var deleteIntentStatus string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT id, status FROM openrails.rail_intents WHERE merchant_id = $1 AND intent_type = $2 AND subscription_id = $3`,
		fx.merchant, intents.TypeNMIDeleteSubscription, subID).Scan(&deleteIntentID, &deleteIntentStatus))
	assert.Equal(t, intents.StatusPending, deleteIntentStatus, "delete rides the ledger (queue-always #679)")

	// Refund executed against the gateway through the refund intent, recorded
	// as a completed negative payment linked to the original.
	assert.EqualValues(t, 1, fx.fake.refundCalls.Load())
	var refundIntentStatus string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.rail_intents WHERE merchant_id = $1 AND intent_type = $2 AND payment_id = $3`,
		fx.merchant, intents.TypeNMIRefund, payID).Scan(&refundIntentStatus))
	assert.Equal(t, intents.StatusSucceeded, refundIntentStatus)
	var refundAmount int64
	var refundStatus, refundTxn string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT amount, status, transaction_id FROM openrails.payments WHERE refunded_payment_id = $1`, payID).
		Scan(&refundAmount, &refundStatus, &refundTxn))
	assert.EqualValues(t, -10000000, refundAmount)
	assert.Equal(t, "completed", refundStatus)
	assert.Equal(t, "txn_refund_1", refundTxn)

	// Finding row: attribution + execution evidence under evidence.resolution.
	row := fx.findingRow(dupID)
	require.NotNil(t, row.ResolvedBy)
	assert.Equal(t, fx.adminID, *row.ResolvedBy)
	assert.Contains(t, string(row.Evidence), `"resolution"`)
	assert.Contains(t, string(row.Evidence), `"cancel_and_refund"`)

	// Queue view: duplicate gauge back to zero, only the orphan remains open.
	after := fx.list("")
	require.Len(t, after.Items, 1)
	assert.EqualValues(t, 0, after.Gauges.DuplicateCoverage)
	assert.EqualValues(t, 1, after.Gauges.Freeloaders)

	// The durable delete drains on the scheduled executor.
	_, err := fx.deleteRunner().RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.rail_intents WHERE id = $1`, deleteIntentID).Scan(&deleteIntentStatus))
	assert.Equal(t, intents.StatusSucceeded, deleteIntentStatus)
	assert.EqualValues(t, 1, fx.fake.deleteCalls.Load())

	// SELF-VERIFYING RESOLUTION: the next converge sweep re-derives truth for
	// the merchant; the fixed finding does NOT reopen (condition gone).
	_, err = converge.NewConvergeEngine(fx.dbi).Converge(fx.ctx, converge.Scope{Merchant: merchant.ID(fx.merchant)})
	require.NoError(t, err)
	row = fx.findingRow(dupID)
	assert.Equal(t, "fixed", row.Status, "converge sweep must not reopen a fixed finding whose condition is gone")

	// Idempotence: approve on an already-resolved finding is a 409 no-op.
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+dupID.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "again"}, dupID.String())
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.EqualValues(t, 1, fx.fake.refundCalls.Load(), "no second money movement")
}

// TestFindingsQueueApprovePartialFailureLeavesOpen: the refund leg fails
// terminally at the gateway → the finding stays OPEN with the error AND the
// completed compensation state (the cancel) in notes; the cancel is not
// silently lost.
func TestFindingsQueueApprovePartialFailureLeavesOpen(t *testing.T) {
	fx := newFindingsFixture(t)
	subID := fx.seedActiveSubscription("psid-" + uuid.NewString()[:8])
	payID := fx.seedCompletedPayment("txn-"+uuid.NewString()[:8], &subID)
	findingID := fx.seedFinding("consistency.duplicate.ownership", "subject-"+uuid.NewString()[:8], "critical",
		"duplicate; cancel + refund", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{"subscription_id": subID.String(), "refund_payment_id": payID.String()},
		})
	fx.fake.refundStatus.Store(`{"object":"transaction","id":"txn_refund_1","response":"2","response_text":"DECLINED","response_code":"300"}`)

	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "dup"}, findingID.String())
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

	row := fx.findingRow(findingID)
	assert.Equal(t, "requires_review", row.Status, "partial failure leaves the finding OPEN")
	assert.Nil(t, row.ResolvedAt)
	require.NotNil(t, row.OperatorNotes)
	assert.Contains(t, *row.OperatorNotes, "failed")
	assert.Contains(t, *row.OperatorNotes, "refund")
	assert.Contains(t, *row.OperatorNotes, `"cancel":"cancelled"`, "compensation state documented")

	// The cancel side is NOT silently lost: sub cancelled + delete intent queued.
	var subStatus string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT status FROM openrails.subscriptions WHERE id = $1`, subID).Scan(&subStatus))
	assert.Equal(t, "cancelled", subStatus)
	var n int
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT count(*) FROM openrails.rail_intents WHERE merchant_id = $1 AND intent_type = $2 AND subscription_id = $3`,
		fx.merchant, intents.TypeNMIDeleteSubscription, subID).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestFindingsQueueIgnoreRequiresNotesAndSilences: ignore without notes is
// rejected; with notes the subject is permanently silenced — a re-emitting
// detector upsert leaves it ignored (same semantics the breaker's dismiss
// honors).
func TestFindingsQueueIgnoreRequiresNotesAndSilences(t *testing.T) {
	fx := newFindingsFixture(t)
	subject := "ent:" + uuid.NewString()
	findingID := fx.seedFinding("derive.entitlement.unjustified", subject, "high", "freeloader", nil)

	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{"outcome": "ignore", "notes": "   "}, findingID.String())
	assert.Equal(t, http.StatusBadRequest, rec.Code, "ignore requires non-empty notes")

	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{"outcome": "ignore", "notes": "known-legitimate comp account"}, findingID.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved resolveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, "ignored", resolved.Finding.Status)
	assert.Equal(t, fx.adminID, resolved.Finding.ResolvedBy)

	// Detector re-emits the same identity: stays ignored (permanent silence).
	reupserted := fx.seedFinding("derive.entitlement.unjustified", subject, "high", "freeloader", nil)
	assert.Equal(t, findingID, reupserted, "stable identity per (merchant, type, subject)")
	row := fx.findingRow(findingID)
	assert.Equal(t, "ignored", row.Status)

	// Gauge: an ignored finding no longer counts.
	assert.EqualValues(t, 0, fx.list("").Gauges.Freeloaders)

	// Approve on a finding with NO structured recommendation is 422 (on a
	// fresh open one), and resolve on the ignored one is a 409 no-op.
	fresh := fx.seedFinding("derive.entitlement.unjustified", "ent:"+uuid.NewString(), "high", "freeloader", nil)
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+fresh.String()+"/resolve",
		map[string]any{"outcome": "approve"}, fresh.String())
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, "requires_review", fx.findingRow(fresh).Status)

	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{"outcome": "ignore", "notes": "again"}, findingID.String())
	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestFindingsQueueOverrideParamsSwapSubscription: the structured params are
// the DEFAULT; override_params flip which subscription dies before approving.
func TestFindingsQueueOverrideParamsSwapSubscription(t *testing.T) {
	fx := newFindingsFixture(t)
	subA := fx.seedActiveSubscription("psid-a-" + uuid.NewString()[:8])
	product2, price2 := fx.seedSecondProduct()
	subB := fx.seedActiveSubscriptionFor(product2, price2, "psid-b-"+uuid.NewString()[:8])
	findingID := fx.seedFinding("consistency.duplicate.ownership", "subject-"+uuid.NewString()[:8], "critical",
		"cancel the later-created (A) by default", &recommend.Recommendation{
			Action: recommend.ActionCancelAndRefund,
			Params: map[string]any{"subscription_id": subA.String()},
		})

	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+findingID.String()+"/resolve",
		map[string]any{
			"outcome":         "approve",
			"notes":           "keep A (annual plan the user meant to keep); cancel B instead",
			"override_params": map[string]any{"subscription_id": subB.String()},
		}, findingID.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var statusA, statusB string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT status FROM openrails.subscriptions WHERE id = $1`, subA).Scan(&statusA))
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT status FROM openrails.subscriptions WHERE id = $1`, subB).Scan(&statusB))
	assert.Equal(t, "active", statusA, "override spared the default target")
	assert.Equal(t, "cancelled", statusB, "override swapped which subscription is cancelled")
}

// TestFindingsQueueRevokeEntitlementAndAdminGrant: the freeloader pair —
// revoke_entitlement (as-of, via the entitlement service) and
// record_admin_grant (idempotent admin-sourced grant via the grants machinery).
func TestFindingsQueueRevokeEntitlementAndAdminGrant(t *testing.T) {
	fx := newFindingsFixture(t)

	var entID uuid.UUID
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`INSERT INTO openrails.entitlements (merchant_id, customer_id, entitlement, start_at, end_at, source_type, source_id)
		 VALUES ($1, $2, 'premium', now() - interval '1 day', NULL, 'admin', $3) RETURNING id`,
		fx.merchant, fx.customer, uuid.New()).Scan(&entID))

	asOf := time.Now().UTC().Truncate(time.Second)
	revokeFinding := fx.seedFinding("derive.entitlement.unjustified", "ent:"+entID.String(), "high",
		"live access with no justification — revoke unless known-legitimate", &recommend.Recommendation{
			Action: recommend.ActionRevokeEntitlement,
			Params: map[string]any{"entitlement_id": entID.String(), "as_of": asOf.Format(time.RFC3339)},
			Alternatives: []recommend.Recommendation{{
				Action: recommend.ActionRecordAdminGrant,
				Params: map[string]any{"customer_id": fx.customer.String(), "product_id": fx.product.String(), "reason": "known-legitimate"},
			}},
		})

	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+revokeFinding.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "no justification found"}, revokeFinding.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var revokedAt *time.Time
	var revokeReason *string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT revoked_at, revoke_reason FROM openrails.entitlements WHERE id = $1`, entID).Scan(&revokedAt, &revokeReason))
	require.NotNil(t, revokedAt, "entitlement revoked")
	require.NotNil(t, revokeReason)
	assert.Equal(t, "admin", *revokeReason)
	assert.WithinDuration(t, asOf, *revokedAt, 2*time.Second, "as-of revoke instant honored")

	// record_admin_grant on a second freeloader finding.
	grantFinding := fx.seedFinding("derive.entitlement.unjustified", "cust:"+fx.customer.String(), "high",
		"known-legitimate — record an admin grant instead", &recommend.Recommendation{
			Action: recommend.ActionRecordAdminGrant,
			Params: map[string]any{"customer_id": fx.customer.String(), "product_id": fx.product.String(), "reason": "comp account"},
		})
	rec = fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+grantFinding.String()+"/resolve",
		map[string]any{"outcome": "approve", "notes": "comp confirmed with support"}, grantFinding.String())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resolved resolveBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
	assert.Equal(t, true, resolved.Execution["grant_created"])

	// Ownership lives in the #514 grant ledger (kind=ownership).
	var sourceID string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx,
		`SELECT source_id FROM openrails.grants
		 WHERE merchant_id = $1 AND customer_id = $2 AND product_id = $3
		   AND kind = 'ownership' AND source_type = 'admin' AND event = 'grant'`,
		fx.merchant, fx.customer, fx.product).Scan(&sourceID))
	assert.Equal(t, "finding:"+grantFinding.String(), sourceID, "grant idempotency keyed to the finding")
}

// TestFindingsHeldBulkAckResumeEndToEnd: trip the #679 breaker → the
// held_bulk finding appears in the queue WITH the ack_resume recommendation →
// approve → destructive execution resumes (the held delete drains).
func TestFindingsHeldBulkAckResumeEndToEnd(t *testing.T) {
	fx := newFindingsFixture(t)
	store := intents.NewStore(fx.dbi)
	now := time.Now().UTC()

	// Consume the whole rolling-window budget with already-executed
	// destructive intents (budget = floor with zero active subs).
	for i := 0; i < intents.DestructiveBudgetFloor; i++ {
		subID := uuid.New()
		row, err := store.Enqueue(fx.ctx, intents.EnqueueParams{
			MerchantID:     fx.merchant,
			Provider:       "nmi",
			IntentType:     intents.TypeNMIDeleteSubscription,
			SubscriptionID: &subID,
			Payload:        intents.NMIDeletePayload{UserID: fx.customer.String(), RailSubscriptionID: fmt.Sprintf("spent-%d", i)},
			IdempotencyKey: intents.NMIDeleteIdempotencyKey(subID),
			NextAttemptAt:  now,
			Origin:         intents.OriginSystem,
			OriginReason:   "findings queue held_bulk test (budget consumption)",
		})
		require.NoError(t, err)
		fx.exec(`UPDATE openrails.rail_intents SET status = 'succeeded', executed_at = now() WHERE id = $1`, row.ID)
	}

	// One more destructive delete for a REAL cancelled sub: over budget → held.
	psid := "psid-held-" + uuid.NewString()[:8]
	subID := fx.seedActiveSubscription(psid)
	fx.exec(`UPDATE openrails.subscriptions
	         SET status = 'cancelled', cancelled_at = now(), cancel_type = 'merchant', deletion_scheduled_at = now()
	         WHERE id = $1`, subID)
	held, err := store.Enqueue(fx.ctx, intents.EnqueueParams{
		MerchantID:     fx.merchant,
		Provider:       "nmi",
		IntentType:     intents.TypeNMIDeleteSubscription,
		SubscriptionID: &subID,
		Payload:        intents.NMIDeletePayload{UserID: fx.customer.String(), RailSubscriptionID: psid},
		IdempotencyKey: intents.NMIDeleteIdempotencyKey(subID),
		NextAttemptAt:  now,
		Origin:         intents.OriginAdmin,
		OriginReason:   "findings queue held_bulk test",
	})
	require.NoError(t, err)

	runner := fx.deleteRunner()
	_, err = runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	var heldStatus string
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT status FROM openrails.rail_intents WHERE id = $1`, held.ID).Scan(&heldStatus))
	require.Equal(t, intents.StatusPending, heldStatus, "over-budget destructive intent held")
	assert.Zero(t, fx.fake.deleteCalls.Load())

	// The held_bulk finding is in the queue with the ack_resume recommendation.
	body := fx.list("?finding_type=" + intents.HeldBulkFindingType)
	require.Len(t, body.Items, 1)
	item := body.Items[0]
	assert.Equal(t, "critical", item.Severity)
	require.NotNil(t, item.Recommendation, "held_bulk carries the structured ack_resume recommendation")
	assert.Equal(t, recommend.ActionAckResume, item.Recommendation.Action)

	// Approve (ack): plain resolution; the breaker re-arms off the status.
	rec := fx.do(AdminResolveFinding, http.MethodPost, "/findings/"+item.ID+"/resolve",
		map[string]any{"outcome": "approve", "notes": "cohort reviewed — routine churn"}, item.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fid := uuid.MustParse(item.ID)
	row := fx.findingRow(fid)
	assert.Equal(t, "fixed", row.Status)
	require.NotNil(t, row.ResolvedBy)
	assert.Equal(t, fx.adminID, *row.ResolvedBy)

	// Destructive execution RESUMES: the held delete drains on the next pass.
	fx.exec(`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1`, held.ID)
	_, err = runner.RunExecuteOnce(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT status FROM openrails.rail_intents WHERE id = $1`, held.ID).Scan(&heldStatus))
	assert.Equal(t, intents.StatusSucceeded, heldStatus, "operator approve resumed destructive execution")
	assert.EqualValues(t, 1, fx.fake.deleteCalls.Load())
}
