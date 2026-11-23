//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// limitsCatalog is a Claude-style plan: a five-hour session in weighted
// tokens, a weekly cap, and an add-on that raises the cap.
const limitsCatalog = `schema_version: 1
meters:
  input-tokens: {event_type: chat, value_property: input_tokens, aggregation: sum, group_by: {model: model}}
limits:
  five_hour:
    unit: usage_units
    reset: {kind: session, window_seconds: 18000}
    overrun: admit_while_positive
    rate_cards:
    - meter: input-tokens
      price: {model: per_unit, per_unit: {divide_by: 1000, matrix: {dimension: model, cells: {small: {unit_amount: "1"}, large: {unit_amount: "5"}}}}}
  weekly:
    unit: usage_units
    reset: {kind: fixed, window_seconds: 604800, anchor: first_use}
    overrun: admit_while_positive
products:
  pro: {display_name: Pro, limits: {five_hour: "1000", weekly: "5000"}}
  boost: {display_name: Boost, limits: {weekly: "1500"}}
`

func applyLimits(t *testing.T, client *openrails.Client, document string, params ...billing.ApplyCatalogParams) *billing.CatalogApplicationReceipt {
	t.Helper()
	app, err := catalog.ParseApplicationYAML([]byte(document))
	require.NoError(t, err)
	var p billing.ApplyCatalogParams
	if len(params) > 0 {
		p = params[0]
	}
	receipt, err := client.ApplyCatalog(t.Context(), app, p)
	require.NoError(t, err)
	return receipt
}

func limitProduct(t *testing.T, client *openrails.Client, key string) billing.ProductID {
	t.Helper()
	products, err := client.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{key}})
	require.NoError(t, err)
	require.Len(t, products.Items, 1)
	return products.Items[0].ID
}

func grantLimitProduct(t *testing.T, client *openrails.Client, customer billing.CustomerID, key string, hours int) billing.ProductAccessGrant {
	t.Helper()
	granted, err := client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: uuid.NewString(),
		Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: limitProduct(t, client, key), Hours: ptr(hours), Reason: billing.GrantReasonComp}}})
	require.NoError(t, err)
	require.Len(t, granted, 1)
	return granted[0]
}

func customerLimit(t *testing.T, client *openrails.Client, customer billing.CustomerID, key string) billing.CustomerLimit {
	t.Helper()
	page, err := client.ListCustomerLimits(t.Context(), customer)
	require.NoError(t, err)
	require.Empty(t, page.Next, "one page holds every limit")
	for _, line := range page.Items {
		require.Equal(t, customer, line.CustomerID)
		if line.Limit == key {
			require.Equal(t, line.Capacity-line.Used-line.Held, line.Remaining)
			return line
		}
	}
	t.Fatalf("customer %s has no limit %q", customer, key)
	return billing.CustomerLimit{}
}

func mustLimits(t *testing.T, client *openrails.Client, customer billing.CustomerID) []billing.CustomerLimit {
	t.Helper()
	page, err := client.ListCustomerLimits(t.Context(), customer)
	require.NoError(t, err)
	return page.Items
}

func limitKeys(lines []billing.CustomerLimit) []string {
	out := []string{}
	for _, l := range lines {
		out = append(out, l.Limit)
	}
	return out
}

func requireCode(t *testing.T, err error, code string) *billing.StatusError {
	t.Helper()
	var failure *billing.StatusError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, code, failure.Code, failure.Message)
	return failure
}

func TestUsageLimits(t *testing.T) {
	t.Run("OR-J23-S07/limits-declare-apply-and-refuse-invalid-terms", func(t *testing.T) {
		a := newAuthorityWorld(t)
		receipt := applyLimits(t, a.alphaRemote, limitsCatalog)
		require.Empty(t, receipt.Conflicts)
		five, err := a.alphaRemote.GetLimit(t.Context(), "five_hour")
		require.NoError(t, err)
		require.Equal(t, "usage_units", five.Unit)
		require.Equal(t, catalog.LimitReset{Kind: catalog.ResetSession, WindowSeconds: 18000}, five.Reset)
		require.Equal(t, catalog.OverrunAdmitWhilePositive, five.Overrun)
		require.Len(t, five.RateCards, 1)
		require.Equal(t, "input-tokens", five.RateCards[0].Meter)
		require.EqualValues(t, 1000, five.RateCards[0].Price.PerUnit.DivideBy)
		require.Equal(t, catalog.RoundHalfUp, five.RateCards[0].Price.PerUnit.Round, "a rate card is stored as it prices")
		require.EqualValues(t, 5, five.RateCards[0].Price.PerUnit.Matrix.Cells["large"].UnitAmount)
		require.Equal(t, []billing.LimitProduct{{ProductID: limitProduct(t, a.alphaRemote, "pro"), ProductKey: "pro", Capacity: 1000}}, five.Products)
		weekly, err := a.alphaRemote.GetLimit(t.Context(), "weekly")
		require.NoError(t, err)
		require.Len(t, weekly.Products, 2, "both products give the weekly limit capacity")

		first, err := a.alpha.ListLimits(t.Context(), billing.PageRequest{Limit: 1})
		require.NoError(t, err)
		require.Len(t, first.Items, 1)
		require.Equal(t, "five_hour", first.Items[0].Key)
		require.NotEmpty(t, first.Next)
		second, err := a.alpha.ListLimits(t.Context(), billing.PageRequest{Limit: 1, Cursor: first.Next})
		require.NoError(t, err)
		require.Equal(t, "weekly", second.Items[0].Key)

		daily, err := a.alpha.SetLimit(t.Context(), "daily", billing.SetLimitParams{Unit: "messages", Reset: catalog.LimitReset{Kind: catalog.ResetFixed, WindowSeconds: 86400, Anchor: catalog.AnchorFirstUse}})
		require.NoError(t, err)
		require.Equal(t, catalog.OverrunRequireEstimate, daily.Overrun, "an edit's overrun defaults to the safe one")
		require.EqualValues(t, 1, daily.Revision)
		require.Empty(t, daily.RateCards)
		require.Empty(t, daily.Products)
		_, err = a.alphaRemote.SetLimit(t.Context(), "daily", billing.SetLimitParams{Unit: "messages", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}, ExpectedRevision: ptr(int64(0))})
		requireCode(t, err, billing.CodeRevisionMismatch)

		// An edit owns the field it set: a document naming it differently skips
		// the limit, and force applies it.
		edited, err := a.alphaRemote.SetLimit(t.Context(), "weekly", billing.SetLimitParams{Unit: weekly.Unit, Reset: weekly.Reset, Overrun: catalog.OverrunRequireEstimate, ExpectedRevision: &weekly.Revision})
		require.NoError(t, err)
		require.Greater(t, edited.Revision, weekly.Revision)
		changed := limitsCatalog + "  extra: {display_name: Extra, limits: {weekly: \"10\"}}\n"
		conflict := applyLimits(t, a.alphaRemote, changed)
		require.Len(t, conflict.Conflicts, 1)
		require.Equal(t, billing.CatalogObjectLimit, conflict.Conflicts[0].Object)
		require.Equal(t, "weekly", conflict.Conflicts[0].Key)
		require.Len(t, conflict.Conflicts[0].Fields, 1)
		require.Equal(t, "overrun", conflict.Conflicts[0].Fields[0].Field)
		kept, err := a.alphaRemote.GetLimit(t.Context(), "weekly")
		require.NoError(t, err)
		require.Equal(t, catalog.OverrunRequireEstimate, kept.Overrun)
		require.Len(t, kept.Products, 3, "the rest of the document applied")
		forced := applyLimits(t, a.alphaRemote, changed, billing.ApplyCatalogParams{Force: true})
		require.Empty(t, forced.Conflicts)
		kept, err = a.alphaRemote.GetLimit(t.Context(), "weekly")
		require.NoError(t, err)
		require.Equal(t, catalog.OverrunAdmitWhilePositive, kept.Overrun)

		for _, invalid := range []string{
			"schema_version: 1\nlimits:\n  balance: {unit: credits, reset: {kind: lifetime}}\n",
			"schema_version: 1\nlimits:\n  spend: {unit: USD, reset: {kind: lifetime}, rate_cards: [{meter: input-tokens, price: {model: per_unit, per_unit: {unit_amount: \"1\"}}}]}\n",
			"schema_version: 1\nlimits:\n  hourly: {unit: requests, reset: {kind: fixed, window_seconds: 3600}}\n",
			"schema_version: 1\nlimits:\n  forever: {unit: requests, reset: {kind: lifetime, window_seconds: 60}}\n",
			"schema_version: 1\nproducts:\n  pro: {limits: {weekly: \"-1\"}}\n",
		} {
			_, err := catalog.ParseApplicationYAML([]byte(invalid))
			require.Error(t, err, invalid)
		}
		for key, params := range map[string]billing.SetLimitParams{
			"balance":   {Unit: "credits", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}},
			"spend":     {Unit: "USD", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}, RateCards: []catalog.LimitRateCard{{Meter: "input-tokens", Price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}}}},
			"unmetered": {Unit: "requests", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}, RateCards: []catalog.LimitRateCard{{Meter: "undeclared", Price: catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}}}},
			"priced":    {Unit: "requests", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}, RateCards: []catalog.LimitRateCard{{Meter: "input-tokens", Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: 1}}}}},
			"shouting":  {Unit: "Requests", Reset: catalog.LimitReset{Kind: catalog.ResetLifetime}},
		} {
			_, err := a.alphaRemote.SetLimit(t.Context(), key, params)
			require.ErrorIs(t, err, billing.ErrInvalid, key)
			_, err = a.alphaRemote.GetLimit(t.Context(), key)
			require.ErrorIs(t, err, billing.ErrNotFound, "a refused declaration stores nothing: %s", key)
		}
		document, err := catalog.ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  ghost: {display_name: Ghost, limits: {undeclared: \"1\"}}\n"))
		require.NoError(t, err)
		_, err = a.alphaRemote.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
		require.ErrorIs(t, err, billing.ErrInvalid, "a product gives capacity only to a declared limit")
		ghosts, err := a.alphaRemote.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"ghost"}})
		require.NoError(t, err)
		require.Empty(t, ghosts.Items, "the refused document applied nothing")

		_, err = a.alphaRemote.GetLimit(t.Context(), "missing")
		requireCode(t, err, billing.CodeLimitNotFound)
		_, err = a.betaRemote.GetLimit(t.Context(), "five_hour")
		requireCode(t, err, billing.CodeLimitNotFound)
		foreign, err := a.betaRemote.ListLimits(t.Context(), billing.PageRequest{})
		require.NoError(t, err)
		require.Empty(t, foreign.Items, "another merchant's catalog declares none of them")
	})

	t.Run("OR-J23-S08/capacity-sums-the-customers-live-products", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, limitsCatalog)
		customer := billing.CustomerID(uuid.New())
		none, err := w.alpha.ListCustomerLimits(t.Context(), customer)
		require.NoError(t, err)
		require.Empty(t, limitKeys(none.Items), "a customer no product or override gives capacity is subject to no limit")
		boosted := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, boosted, "boost", 24)
		only, err := w.alpha.ListCustomerLimits(t.Context(), boosted)
		require.NoError(t, err)
		require.Equal(t, []string{"weekly"}, limitKeys(only.Items), "a customer is subject only to the limits it is granted")
		pro := grantLimitProduct(t, w.alpha, customer, "pro", 24*30)
		grantLimitProduct(t, w.alpha, customer, "boost", 24)
		page, err := w.alpha.ListCustomerLimits(t.Context(), customer)
		require.NoError(t, err)
		require.Equal(t, []string{"five_hour", "weekly"}, limitKeys(page.Items), "every limit the customer is granted, by key")
		weekly := customerLimit(t, w.alpha, customer, "weekly")
		require.Equal(t, billing.CustomerLimit{CustomerID: customer, Limit: "weekly", Unit: "usage_units", Capacity: 6500, Remaining: 6500}, weekly,
			"two live products add up; a first-use window opens at first use")
		require.EqualValues(t, 1000, customerLimit(t, w.alpha, customer, "five_hour").Capacity)
		w.clock.Advance(25 * time.Hour)
		require.EqualValues(t, 5000, customerLimit(t, w.alpha, customer, "weekly").Capacity, "a lapsed product gives nothing")
		require.NoError(t, w.alpha.RevokeProductAccess(t.Context(), pro.ID, billing.RevokeProductAccessParams{Reason: "plan ended"}))
		ended, err := w.alpha.ListCustomerLimits(t.Context(), customer)
		require.NoError(t, err)
		require.Empty(t, limitKeys(ended.Items), "a revoked product gives nothing, and leaves the customer subject to no limit")
		other := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, other, "boost", 24)
		require.EqualValues(t, 1500, customerLimit(t, w.alpha, other, "weekly").Capacity, "capacity is per customer")
	})

	t.Run("OR-J23-S09/an-override-replaces-the-products-capacity", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, limitsCatalog)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "pro", 24*30)
		grantLimitProduct(t, w.alpha, customer, "boost", 24*30)
		require.EqualValues(t, 6500, customerLimit(t, w.alpha, customer, "weekly").Capacity)
		saved, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "weekly", Capacity: 200}}})
		require.NoError(t, err)
		require.Equal(t, []billing.LimitOverride{{Limit: "weekly", Capacity: 200}}, saved.Settings.Limits)
		weekly := customerLimit(t, w.alpha, customer, "weekly")
		require.EqualValues(t, 200, weekly.Capacity, "an override replaces the products' sum")
		require.EqualValues(t, 200, weekly.Remaining)
		require.EqualValues(t, 1000, customerLimit(t, w.alpha, customer, "five_hour").Capacity, "a limit with no override keeps its products' capacity")
		_, err = w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "weekly", Capacity: 0}}})
		require.NoError(t, err)
		require.Zero(t, customerLimit(t, w.alpha, customer, "weekly").Capacity, "an override of zero gives none")
		saved, err = w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "weekly", Inherit: true}}})
		require.NoError(t, err)
		require.Empty(t, saved.Settings.Limits)
		require.EqualValues(t, 6500, customerLimit(t, w.alpha, customer, "weekly").Capacity, "inherit restores the products' sum")

		_, err = w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: []billing.LimitOverride{{Limit: "undeclared", Capacity: 1}}})
		failure := requireCode(t, err, billing.CodeLimitNotFound)
		require.NotNil(t, failure.Param)
		require.Equal(t, "limits[0].limit", *failure.Param)
		for _, invalid := range [][]billing.LimitOverride{
			{{Limit: "balance", Capacity: 1}},
			{{Limit: "weekly", Capacity: -1}},
			{{Limit: "weekly", Inherit: true, Capacity: 5}},
			{{Limit: "weekly", Capacity: 1}, {Limit: "weekly", Capacity: 2}},
		} {
			_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{Limits: invalid})
			require.ErrorIs(t, err, billing.ErrInvalid)
		}
		require.EqualValues(t, 6500, customerLimit(t, w.alpha, customer, "weekly").Capacity, "a refused change writes nothing")
	})

	t.Run("OR-J23-S10/an-adjustment-adds-to-the-current-window-only", func(t *testing.T) {
		w := newWorld(t)
		applyLimits(t, w.alpha, `schema_version: 1
limits:
  hourly: {unit: requests, reset: {kind: fixed, window_seconds: 3600, anchor: first_use}}
  session: {unit: requests, reset: {kind: session, window_seconds: 600}}
  ever: {unit: requests, reset: {kind: lifetime}}
  burst: {unit: requests, reset: {kind: rate, window_seconds: 60}}
products:
  plan: {display_name: Plan, limits: {hourly: "100", session: "10", ever: "1000", burst: "5"}}
`)
		customer := billing.CustomerID(uuid.New())
		grantLimitProduct(t, w.alpha, customer, "plan", 24*30)
		require.Nil(t, customerLimit(t, w.alpha, customer, "hourly").WindowStartsAt, "no use, no window")
		opened := w.clock.Now().UTC().Truncate(time.Second)
		adjusted, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: 50, SourceID: "adjust-1"})
		require.NoError(t, err)
		require.EqualValues(t, 150, adjusted.Capacity)
		require.EqualValues(t, 150, adjusted.Remaining)
		require.NotNil(t, adjusted.WindowStartsAt)
		require.True(t, opened.Equal(*adjusted.WindowStartsAt), "the adjustment opens the window: %s", adjusted.WindowStartsAt)
		require.True(t, opened.Add(time.Hour).Equal(*adjusted.ResetsAt))
		adjusted, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: -20, SourceID: "adjust-2"})
		require.NoError(t, err)
		require.EqualValues(t, 130, adjusted.Capacity, "adjustments add up in the window")
		retried, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: -20, SourceID: "adjust-2"})
		require.NoError(t, err)
		require.Equal(t, adjusted, retried, "a retry with the same source_id answers the line and adds nothing")
		_, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: -25, SourceID: "adjust-2"})
		requireCode(t, err, billing.CodeIdempotencyKeyReused)
		_, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: 5})
		require.ErrorIs(t, err, billing.ErrInvalid, "an adjustment names its source_id")
		require.EqualValues(t, 130, customerLimit(t, w.alpha, customer, "hourly").Capacity, "neither changed the window")
		w.clock.Advance(30 * time.Minute)
		require.Equal(t, adjusted, customerLimit(t, w.alpha, customer, "hourly"), "the window keeps its adjustments")
		w.clock.Advance(31 * time.Minute)
		next := customerLimit(t, w.alpha, customer, "hourly")
		require.EqualValues(t, 100, next.Capacity, "the next window starts from the products' capacity")
		require.True(t, opened.Add(time.Hour).Equal(*next.WindowStartsAt), "fixed windows tile from the first: %s", next.WindowStartsAt)
		require.True(t, opened.Add(2*time.Hour).Equal(*next.ResetsAt))

		ever, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "ever", Amount: 500, SourceID: "adjust-3"})
		require.NoError(t, err)
		require.EqualValues(t, 1500, ever.Capacity)
		require.Nil(t, ever.WindowStartsAt)
		require.Nil(t, ever.ResetsAt)
		session, err := w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "session", Amount: 5, SourceID: "adjust-4"})
		require.NoError(t, err)
		require.EqualValues(t, 15, session.Capacity)
		require.NotNil(t, session.WindowStartsAt, "an adjustment opens a session")
		w.clock.Advance(48 * time.Hour)
		require.EqualValues(t, 1500, customerLimit(t, w.alpha, customer, "ever").Capacity, "a lifetime limit has one window")
		ended := customerLimit(t, w.alpha, customer, "session")
		require.EqualValues(t, 10, ended.Capacity, "an ended session's adjustment ends with it")
		require.Nil(t, ended.WindowStartsAt)

		_, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "burst", Amount: 5, SourceID: "adjust-5"})
		require.ErrorIs(t, err, billing.ErrInvalid, "a rate limit refills on its own")
		_, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "hourly", Amount: 0, SourceID: "adjust-6"})
		require.ErrorIs(t, err, billing.ErrInvalid)
		_, err = w.alpha.AdjustCustomerLimit(t.Context(), customer, billing.AdjustLimitParams{Limit: "undeclared", Amount: 5, SourceID: "adjust-7"})
		requireCode(t, err, billing.CodeLimitNotFound)
		require.EqualValues(t, 5, customerLimit(t, w.alpha, customer, "burst").Capacity)
	})

	t.Run("OR-J23-S11/limit-reads-stay-within-their-merchant-and-customer", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alphaRemote, limitsCatalog)
		applyLimits(t, a.betaRemote, "schema_version: 1\nlimits:\n  weekly: {unit: usage_units, reset: {kind: fixed, window_seconds: 604800, anchor: first_use}}\nproducts:\n  basic: {display_name: Basic, limits: {weekly: \"7\"}}\n")
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		grantLimitProduct(t, a.alphaRemote, alice, "pro", 24*30)
		_, err := a.alphaRemote.AdjustCustomerLimit(t.Context(), alice, billing.AdjustLimitParams{Limit: "weekly", Amount: 100, SourceID: "adjust-8"})
		require.NoError(t, err)
		require.EqualValues(t, 5100, customerLimit(t, a.alphaRemote, alice, "weekly").Capacity)

		foreign, err := a.betaRemote.ListCustomerLimits(t.Context(), alice)
		require.NoError(t, err)
		require.Empty(t, foreign.Items, "another merchant's products and adjustments count nowhere else")
		grantLimitProduct(t, a.betaRemote, alice, "basic", 24*30)
		_, err = a.betaRemote.AdjustCustomerLimit(t.Context(), alice, billing.AdjustLimitParams{Limit: "weekly", Amount: 3, SourceID: "adjust-9"})
		require.NoError(t, err)
		require.Equal(t, []string{"weekly"}, limitKeys(mustLimits(t, a.betaRemote, alice)), "a merchant reads its own limits only")
		require.EqualValues(t, 10, customerLimit(t, a.betaRemote, alice, "weekly").Capacity)
		require.EqualValues(t, 5100, customerLimit(t, a.alphaRemote, alice, "weekly").Capacity)
		_, err = a.betaRemote.AdjustCustomerLimit(t.Context(), alice, billing.AdjustLimitParams{Limit: "five_hour", Amount: 3, SourceID: "adjust-10"})
		requireCode(t, err, billing.CodeLimitNotFound)
		require.Empty(t, mustLimits(t, a.alphaRemote, bob), "a customer's capacity is its own")

		reader, err := a.auth.CreateAPIKey(t.Context(), iam.SystemIdentity(), a.alphaGroup, iam.NewAPIKey{Name: "customer-reader", Role: a.reader})
		require.NoError(t, err)
		list := "/v1/app/limits?" + url.Values{"customer_id": {alice.String()}}.Encode()
		for _, check := range []struct {
			token  string
			status int
		}{{"", 401}, {a.aliceToken, 403}, {reader.Secret, 403}} {
			status, raw := a.request(t, http.MethodGet, list, check.token, nil)
			require.Equal(t, check.status, status, string(raw))
		}
		adjust := "/v1/admin/customers/" + alice.String() + "/limit-adjustments"
		for _, check := range []struct {
			token  string
			status int
		}{{"", 401}, {a.bobToken, 403}, {reader.Secret, 403}} {
			status, raw := a.request(t, http.MethodPost, adjust, check.token, billing.AdjustLimitParams{Limit: "weekly", Amount: 1000, SourceID: "adjust-11"})
			require.Equal(t, check.status, status, string(raw))
		}
		require.EqualValues(t, 5100, customerLimit(t, a.alphaRemote, alice, "weekly").Capacity, "a refused adjustment adds nothing")
	})

	t.Run("OR-J23-S12/a-customer-reads-its-own-limits", func(t *testing.T) {
		a := newAuthorityWorld(t)
		applyLimits(t, a.alphaRemote, limitsCatalog)
		alice, bob := billing.CustomerID(uuid.MustParse(a.alice.ID)), billing.CustomerID(uuid.MustParse(a.bob.ID))
		grantLimitProduct(t, a.alphaRemote, alice, "pro", 24*30)
		_, err := a.alphaRemote.AdjustCustomerLimit(t.Context(), alice, billing.AdjustLimitParams{Limit: "weekly", Amount: 250, SourceID: "adjust-12"})
		require.NoError(t, err)
		staff, err := a.alphaRemote.ListCustomerLimits(t.Context(), alice)
		require.NoError(t, err)

		status, raw := a.request(t, http.MethodGet, "/v1/me/limits", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var own billing.ListPage[billing.CustomerLimit]
		require.NoError(t, json.Unmarshal(raw, &own))
		require.Equal(t, staff.Items, own.Items, "the customer sees what staff see")
		require.Equal(t, []string{"five_hour", "weekly"}, limitKeys(own.Items))
		require.EqualValues(t, 5250, own.Items[1].Capacity)
		require.Contains(t, string(raw), `"capacity":"5250"`, "units travel as decimal strings")
		require.Contains(t, string(raw), `"next_cursor":null`)

		grantLimitProduct(t, a.alphaRemote, bob, "boost", 24*30)
		status, raw = a.request(t, http.MethodGet, "/v1/me/limits", a.bobToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var bobs billing.ListPage[billing.CustomerLimit]
		require.NoError(t, json.Unmarshal(raw, &bobs))
		require.Equal(t, []string{"weekly"}, limitKeys(bobs.Items), "a customer reads the limits it is granted")
		for _, line := range bobs.Items {
			require.Equal(t, bob, line.CustomerID, "a customer reads only its own limits")
			require.EqualValues(t, 1500, line.Capacity)
		}
		status, raw = a.request(t, http.MethodGet, "/v1/me/limits", "", nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
	})
}
