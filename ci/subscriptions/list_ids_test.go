//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
)

// idsOf is the ids of a list page's records: each item's key field, id when
// key is empty.
func idsOf(t *testing.T, page map[string]any, key ...string) []string {
	t.Helper()
	field := "id"
	if len(key) > 0 && key[0] != "" {
		field = key[0]
	}
	var out []string
	for _, item := range page["data"].([]any) {
		out = append(out, item.(map[string]any)[field].(string))
	}
	return out
}

// cloneElsewhere copies a record to another merchant under a fresh id, as if
// that merchant held it, and answers the copy's wire id. Foreign keys are off
// for the copy: nothing of the other merchant's needs to exist, and the copy
// is gone before the world's invariants run. set adjusts columns a key unique
// across merchants holds.
func (w *world) cloneElsewhere(table, wireID, set string) string {
	w.t.Helper()
	ctx := w.t.Context()
	prefix, raw := "", wireID
	if at := strings.LastIndex(wireID, "_"); at >= 0 {
		prefix, raw = wireID[:at+1], wireID[at+1:]
	}
	var columns []string
	rows, err := w.pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND is_generated = 'NEVER' ORDER BY ordinal_position`, w.schema, table)
	require.NoError(w.t, err)
	columns, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	list := strings.Join(columns, ", ")
	id, merchant := uuid.New(), uuid.New()
	tx, err := w.pool.Begin(ctx)
	require.NoError(w.t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		"SET LOCAL session_replication_role = replica",
		w.q(fmt.Sprintf("CREATE TEMP TABLE clone ON COMMIT DROP AS SELECT %s FROM billing.%s WHERE id = '%s'", list, table, uuid.MustParse(raw))),
		fmt.Sprintf("UPDATE clone SET merchant_id = '%s', id = '%s'%s", merchant, id, set),
		w.q(fmt.Sprintf("INSERT INTO billing.%s (%s) SELECT %s FROM clone", table, list, list)),
	} {
		_, err := tx.Exec(ctx, stmt)
		require.NoError(w.t, err, stmt)
	}
	require.NoError(w.t, tx.Commit(ctx))
	w.t.Cleanup(func() { w.dropElsewhere(table, merchant) })
	return prefix + id.String()
}

// dropElsewhere removes another merchant's copies, past the triggers that
// keep billing facts immutable.
func (w *world) dropElsewhere(table string, merchant uuid.UUID) {
	w.t.Helper()
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	require.NoError(w.t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
	require.NoError(w.t, err)
	_, err = tx.Exec(ctx, w.q("DELETE FROM billing."+table+" WHERE merchant_id = $1"), merchant)
	require.NoError(w.t, err)
	require.NoError(w.t, tx.Commit(ctx))
}

// Every merchant list reads named records by its ids filter: the subset
// asked for, in one page; unknown ids and another merchant's are absent; the
// filter is bounded and takes nothing beside it. A new list fails here until
// it has a fixture.
func TestListsReadNamedRecords(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	client := w.client[embedded]

	// An NMI membership through one renewal: its subscription, payments, card,
	// attempts, rebill cycle and host events.
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	e.toPeriodEnd()
	w.runRenewals()
	c := e.c
	// A reprice batch moving the membership to a new version of its key.
	priceID, err := billing.ParsePriceID(e.price)
	require.NoError(t, err)
	price, err := client.GetPrice(ctx, priceID, billing.GetPriceParams{})
	require.NoError(t, err)
	product, err := client.GetProduct(ctx, price.ProductID)
	require.NoError(t, err)
	hours := monthHours
	_, err = client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: price.ProductID, Key: price.Key, UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)
	_, err = client.CreateRepriceBatch(ctx, billing.CreateRepriceBatchParams{ProductKey: product.Key, PriceKey: price.Key, EffectiveAt: w.clock.Now().Add(45 * 24 * time.Hour)})
	require.NoError(t, err)
	// Two free product grants.
	gift := w.giftProduct("content:gift")
	c.grant(gift, &hours, nil)
	c.grant(gift, &hours, nil)
	// An invoice for usage on credit, paid from the next funding: invoice
	// payments, credit grants and their ledger.
	d := w.newCustomer()
	_, err = w.client[remote].UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{CustomerID: d.cid(), CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}}}})
	require.NoError(t, err)
	_, err = recordUsage(ctx, w.client[remote], billing.RecordUsageParams{CustomerID: d.cid(), Invoker: d.id, Currency: "USD", EventType: "ids", Amount: 50_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	w.advance(time.Minute)
	job, err := w.jobs.Insert(ctx, invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(job.Job.ID)
	invoices, err := client.ListInvoices(ctx, billing.InvoiceListParams{CustomerID: d.cid()})
	require.NoError(t, err)
	require.NotEmpty(t, invoices.Items)
	_, err = client.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{
		{CustomerID: d.cid(), Currency: "USD", Amount: 30_000_000, Source: "support", SourceID: uuid.NewString()},
		{CustomerID: d.cid(), Currency: "USD", Amount: 30_000_000, Source: "support", SourceID: uuid.NewString()},
	})
	require.NoError(t, err)
	// What detectors and alerting leave: an inbox, findings, catalog drift and
	// an alert webhook.
	for _, stmt := range []string{
		`INSERT INTO billing.notifications (merchant_id, recipient_kind, event_type, severity, title, body, data)
			SELECT id, 'merchant', 'operator.alert', 'warning', 'Heads up', 'Something to read', '{}' FROM billing.merchants WHERE slug = $1`,
		`INSERT INTO billing.merchant_webhooks (merchant_id, name, destination_host, secret_version, format, enabled)
			SELECT id, 'ops', 'hooks.example.test', 1, 'generic', true FROM billing.merchants WHERE slug = $1`,
	} {
		for range 2 {
			_, err = w.pool.Exec(ctx, w.q(stmt), w.slug)
			require.NoError(t, err)
		}
	}
	for range 2 {
		_, err = client.CreateProvisioningToken(ctx, billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
	}
	for range 2 {
		w.seedFinding(duplicateCharge, "provider_charge:"+c.id+":"+uuid.NewString()+":"+w.clock.Now().Format("2006-01"))
	}
	stripePSP := w.psp["stripe"].UUID()
	for _, external := range []string{"price_one", "price_two"} {
		_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, rail, psp_id, openrails_resource_type,
				openrails_resource_id, external_resource_id, field, subject_key, severity, status)
			SELECT id, 'catalog.field_drift', 'stripe', $2::uuid, 'price', $3::text, $4::text, 'unit_amount',
				jsonb_build_array($2::uuid::text, 'price', $3::text, $4::text, 'unit_amount')::text, 'medium', 'requires_review'
			FROM billing.merchants WHERE slug = $1`), w.slug, stripePSP, priceID.String(), external)
		require.NoError(t, err)
	}

	customer, funded := "/v1/admin/customers/"+c.id, "/v1/admin/customers/"+d.id
	invoice := invoices.Items[0].ID.String()
	fixtures := map[string]struct{ path, query, table, set, key string }{
		"GET /v1/admin/customers/settings":                           {"/v1/admin/customers/settings", "", "customers", "", "customer_id"},
		"GET /v1/admin/customers":                                    {"/v1/admin/customers", "", "customers", "", ""},
		"GET /v1/admin/catalog/products":                             {"/v1/admin/catalog/products", "", "products", "", ""},
		"GET /v1/admin/catalog/prices":                               {"/v1/admin/catalog/prices", "", "prices", "", ""},
		"GET /v1/admin/catalog/drift":                                {"/v1/admin/catalog/drift", "", "reconciliation_findings", "", ""},
		"GET /v1/admin/subscriptions":                                {"/v1/admin/subscriptions", "", "subscriptions", "", ""},
		"GET /v1/admin/reprice-batches":                              {"/v1/admin/reprice-batches", "", "reprice_batches", "", ""},
		"GET /v1/admin/reprices":                                     {"/v1/admin/reprices", "", "subscription_reprices", "", ""},
		"GET /v1/admin/customers/{customer_id}/product-access":       {customer + "/product-access", "", "product_access", "", ""},
		"GET /v1/admin/customers/{customer_id}/credit-grants":        {funded + "/credit-grants", "", "grants", "", ""},
		"GET /v1/admin/customers/{customer_id}/balance/transactions": {funded + "/balance/transactions", "currency=USD", "ledger_transfers", "", ""},
		"GET /v1/admin/invoices":                                     {"/v1/admin/invoices", "", "invoices", "", ""},
		"GET /v1/admin/invoices/{id}/payments":                       {"/v1/admin/invoices/" + invoice + "/payments", "", "invoice_payments", "", ""},
		"GET /v1/admin/payments":                                     {"/v1/admin/payments", "", "payments", "", ""},
		"GET /v1/admin/payment-attempts":                             {"/v1/admin/payment-attempts", "", "payment_attempts", "", ""},
		"GET /v1/admin/rebill-cycles":                                {"/v1/admin/rebill-cycles", "", "rebill_cycles", "", ""},
		"GET /v1/admin/customers/{customer_id}/payment-methods":      {customer + "/payment-methods", "", "payment_methods", "", ""},
		"GET /v1/admin/customers/{customer_id}/mandates":             {customer + "/mandates", "", "mandates", "", ""},
		"GET /v1/admin/psps":                                         {"/v1/admin/psps", "", "psps", ", account_id = 'elsewhere'", ""},
		"GET /v1/admin/alert-webhooks":                               {"/v1/admin/alert-webhooks", "", "merchant_webhooks", "", ""},
		"GET /v1/admin/provisioning-tokens":                          {"/v1/admin/provisioning-tokens", "", "provisioning_tokens", ", token_sha256 = sha256(token_sha256)", ""},
		"GET /v1/admin/host-events":                                  {"/v1/admin/host-events", "include_acknowledged=true", "host_outbox", "", ""},
		"GET /v1/admin/notifications":                                {"/v1/admin/notifications", "", "notifications", "", ""},
		"GET /v1/admin/findings":                                     {"/v1/admin/findings", "", "reconciliation_findings", "", ""},
	}
	listed := 0
	for _, route := range routes.Catalog() {
		takesIDs := false
		for _, p := range route.Query {
			takesIDs = takesIDs || p.Kind == "ids"
		}
		if !route.Staff() || !takesIDs {
			continue
		}
		fixture, ok := fixtures[route.Key()]
		require.True(t, ok, "%s takes ids and has no fixture", route.Key())
		listed++
		get := func(query string) (int, map[string]any) {
			t.Helper()
			return w.staffJSON(http.MethodGet, fixture.path+"?"+query, nil)
		}

		query := "limit=100"
		if fixture.query != "" {
			query += "&" + fixture.query
		}
		status, page := get(query)
		require.Equal(t, http.StatusOK, status, "%s: %v", route.Key(), page)
		mine := idsOf(t, page, fixture.key)
		require.NotEmpty(t, mine, "%s: the fixture holds records", route.Key())
		ask := mine
		if len(mine) > 1 {
			ask = mine[1:]
		}
		foreign := w.cloneElsewhere(fixture.table, mine[0], fixture.set)
		unknown := foreign[:len(foreign)-36] + uuid.NewString()
		named := append(append([]string{}, ask...), foreign, unknown, ask[0])
		status, page = get("ids=" + strings.Join(named, ","))
		require.Equal(t, http.StatusOK, status, "%s: %v", route.Key(), page)
		require.ElementsMatch(t, ask, idsOf(t, page, fixture.key), "%s answers the named records once, never another merchant's", route.Key())
		require.Nil(t, page["next_cursor"], route.Key())

		for _, refused := range []string{
			"ids=" + strings.Repeat(unknown+",", billing.MaxBatchItems) + unknown,
			"ids=" + ask[0] + "&limit=1",
			"ids=" + ask[0] + "&ids=" + ask[0],
			"ids=",
			"ids=not-an-id",
		} {
			status, body := get(refused)
			require.Equal(t, http.StatusBadRequest, status, "%s?%s: %v", route.Key(), refused, body)
			require.Equal(t, "invalid_query", body["error"].(map[string]any)["code"], route.Key())
			require.Equal(t, "ids", body["error"].(map[string]any)["param"], route.Key())
		}
	}
	require.Len(t, fixtures, listed, "every fixture names a list that takes ids")

	// The same customer id at another merchant is another customer.
	status, page := w.staffJSON(http.MethodGet, "/v1/admin/customers?ids="+c.id, nil)
	require.Equal(t, http.StatusOK, status)
	before := idsOf(t, page)
	tx, err := w.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SET LOCAL session_replication_role = replica")
	require.NoError(t, err)
	elsewhere := uuid.New()
	for _, stmt := range []string{
		`INSERT INTO billing.customers (merchant_id, id) VALUES ($1, $2)`,
		`INSERT INTO billing.customer_contacts (merchant_id, customer_id, email, directory_updated_at) VALUES ($1, $2, 'elsewhere@example.test', now())`,
	} {
		_, err = tx.Exec(ctx, w.q(stmt), elsewhere, c.cid().UUID())
		require.NoError(t, err)
	}
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() { w.dropElsewhere("customer_contacts", elsewhere); w.dropElsewhere("customers", elsewhere) })
	status, page = w.staffJSON(http.MethodGet, "/v1/admin/customers?ids="+c.id, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, before, idsOf(t, page))
	require.Nil(t, page["data"].([]any)[0].(map[string]any)["contact"], "another merchant's contact is not this customer's")
}

// Effective tiers are looked up for many customers at once: every requested
// customer is answered, null when it holds no tier of the group, and another
// merchant's access never counts.
func TestEffectiveTierLookups(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	group := "g" + uuid.NewString()[:8]
	low := w.tierPrice(group, 1, 1000, monthHours, false)
	high := w.tierPrice(group, 2, 2000, monthHours, false)
	hours := monthHours
	one, both, none := w.newCustomer(), w.newCustomer(), w.newCustomer()
	one.grant(low.ProductID, &hours, nil)
	both.grant(low.ProductID, &hours, nil)
	window := both.grant(high.ProductID, &hours, nil)
	unknown := billing.CustomerID(uuid.New())
	// The same window at another merchant, for a customer holding none here.
	w.cloneElsewhere("product_access", window.ID.String(), fmt.Sprintf(", customer_id = '%s'", none.cid().UUID()))

	tiers, err := w.client[remote].GetEffectiveTiers(ctx, billing.GetEffectiveTiersParams{Group: group, CustomerIDs: []billing.CustomerID{one.cid(), both.cid(), none.cid(), unknown, one.cid()}})
	require.NoError(t, err)
	require.Len(t, tiers, 4, "every requested customer is answered once")
	require.Equal(t, low.ent, tiers[one.cid()].Entitlement)
	require.Equal(t, 1, tiers[one.cid()].TierRank)
	require.Equal(t, high.ent, tiers[both.cid()].Entitlement, "the highest-ranked product wins")
	require.Equal(t, high.ProductID, tiers[both.cid()].ProductID)
	require.Contains(t, tiers, none.cid())
	require.Nil(t, tiers[none.cid()], "another merchant's access never counts")
	require.Contains(t, tiers, unknown)
	require.Nil(t, tiers[unknown], "an unknown customer holds no tier")

	ids := make([]string, billing.MaxBatchItems+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	for _, body := range []map[string]any{{"group": group, "customer_ids": ids}, {"group": group, "customer_ids": []string{}}, {"customer_ids": ids[:1]}} {
		status, refused := w.staffJSON(http.MethodPost, "/v1/admin/tiers/lookup", body)
		require.Equal(t, http.StatusBadRequest, status, "%v", refused)
		require.Equal(t, "invalid_param", refused["error"].(map[string]any)["code"])
	}
}

// The single forms the batches replaced are gone, every batch route refuses
// more items than its bound, and a credit-grant batch answers 201, or 200
// when it only replays.
func TestBatchRoutesReplaceTheSingles(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/v1/admin/customers/lookup"},
		{http.MethodGet, "/v1/admin/customers/" + c.id + "/tier?group=g"},
		{http.MethodPut, "/v1/admin/customers/" + c.id + "/spend-delegations/invoker/someone"},
		{http.MethodPost, "/v1/admin/customers/" + c.id + "/credit-grants"},
		{http.MethodPost, "/v1/admin/admissions/req-1/release"},
		{http.MethodPost, "/v1/admin/admissions/req-1/extend"},
	} {
		status, body := w.staff(probe.method, probe.path)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status, "%s %s: %s", probe.method, probe.path, body)
	}
	status, body := w.callAt(w.server.URL, c.token, http.MethodPost, "/notifications/"+billing.NotificationID(uuid.New()).String()+"/read", "", map[string]any{})
	require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status, "%v", body)

	many := func(n int, item func(int) any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = item(i)
		}
		return out
	}
	expires := w.clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for path, body := range map[string]map[string]any{
		"/v1/admin/admissions/release": {"request_ids": many(billing.MaxAdmissionBatchItems+1, func(i int) any { return fmt.Sprint("r", i) })},
		"/v1/admin/admissions/extend": {"items": many(billing.MaxAdmissionBatchItems+1, func(i int) any {
			return map[string]any{"request_id": fmt.Sprint("r", i), "expires_at": expires}
		})},
		"/v1/admin/wasted-spend": {"items": many(billing.MaxBatchItems+1, func(i int) any {
			return map[string]any{"customer_id": c.id, "invoker": c.id, "currency": "USD", "amount": "1", "source": "s", "source_id": fmt.Sprint(i)}
		})},
		"/v1/admin/credit-grants": {"items": many(billing.MaxBatchItems+1, func(i int) any {
			return map[string]any{"customer_id": c.id, "currency": "USD", "amount": "1", "source": "s", "source_id": fmt.Sprint(i)}
		})},
	} {
		status, refused := w.hostJSON(http.MethodPost, path, body)
		require.Equal(t, http.StatusBadRequest, status, "%s: %v", path, refused)
		require.Equal(t, "invalid_param", refused["error"].(map[string]any)["code"], path)
	}
	status, body = w.callAt(w.server.URL, c.token, http.MethodPost, "/notifications/read", "", map[string]any{
		"notification_ids": many(billing.MaxBatchItems+1, func(int) any { return billing.NotificationID(uuid.New()).String() }),
	})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)

	grant := map[string]any{"items": []any{map[string]any{"customer_id": c.id, "currency": "USD", "amount": "1000", "source": "support", "source_id": uuid.NewString()}}}
	status, body = w.staffJSON(http.MethodPost, "/v1/admin/credit-grants", grant)
	require.Equal(t, http.StatusCreated, status, "%v", body)
	status, body = w.staffJSON(http.MethodPost, "/v1/admin/credit-grants", grant)
	require.Equal(t, http.StatusOK, status, "a replay creates nothing: %v", body)
	require.Equal(t, true, body["items"].([]any)[0].(map[string]any)["replayed"])

	// A signed-in admin's batch counts every item against the grant limit.
	items := many(9, func(int) any {
		return map[string]any{"customer_id": c.id, "currency": "USD", "amount": "1", "source": "support", "source_id": uuid.NewString()}
	})
	status, body = w.staffJSON(http.MethodPost, "/v1/admin/credit-grants", map[string]any{"items": items})
	require.Equal(t, http.StatusTooManyRequests, status, "two grants and nine more pass ten a minute: %v", body)
	require.Equal(t, "rate_limit_exceeded", body["error"].(map[string]any)["code"])
}
