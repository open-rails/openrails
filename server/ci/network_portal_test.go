//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/server/internal/operator"
)

// network calls the account's own API.
func (w *world) network(method, path, authorization string, body any) *httptest.ResponseRecorder {
	w.t.Helper()
	q := rsRequest{method: method, path: "/v1/network" + path, authorization: authorization}
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		q.body = string(raw)
	}
	return serve(w.handler, q)
}

// networkList is every page of a network list, read limit at a time.
func networkList[T any](w *world, path, authorization string, limit int) []T {
	w.t.Helper()
	var out []T
	cursor := ""
	for range 50 {
		r := w.network(http.MethodGet, path+"?limit="+strconv.Itoa(limit)+"&cursor="+url.QueryEscape(cursor), authorization, nil)
		require.Equal(w.t, http.StatusOK, r.Code, r.Body.String())
		page := decode[billing.ListPage[T]](w.t, r)
		out = append(out, page.Items...)
		if page.Next == "" {
			return out
		}
		cursor = page.Next
	}
	w.t.Fatal("the list never ends")
	return nil
}

// providerRun imports an NMI-run schedule of the merchant's customer: NMI
// bills it on its own and OpenRails follows.
func (w *world) providerRun(mid billing.MerchantID, customer billing.CustomerID) billing.SubscriptionID {
	w.t.Helper()
	ctx, shop := w.t.Context(), handle(w.t, w.srv, mid)
	plan := "legacy_plan_" + uuid.NewString()[:8]
	gateway := w.gatewayOf(mid)
	gateway.AddPlan(nmimock.Plan{ID: plan, Name: "Legacy " + plan, Amount: "9.99", Days: 30})
	product, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: "legacy-" + uuid.NewString()[:8], DisplayName: "Legacy"})
	require.NoError(w.t, err)
	hours := 720
	price, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", Currency: "USD", UnitAmount: 9_990_000,
		BillingIntervalHours: &hours, AccessDurationHours: &hours, PSPs: map[string]map[string]string{"nmi": {"plan_id": plan}}})
	require.NoError(w.t, err)
	end := w.now().UTC().Add(20 * 24 * time.Hour).Truncate(24 * time.Hour)
	start := end.Add(-time.Duration(hours) * time.Hour)
	vault := gateway.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	schedule := gateway.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: "9.99", Days: 30, NextBilling: end})
	paid := gateway.AddScheduleSale(schedule, start)
	method := &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: gateway.Vault(vault).BillingID}
	brand, last4, month, year := "visa", "1111", 12, 2035
	result, err := shop.ImportBilling(ctx, billing.DeclaredBilling{AsOf: w.now(), DefaultPSP: billing.PSPRef{Key: "nmi"},
		Customers: []billing.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: method.RailMethodRef,
			Card: &billing.CardDetails{Brand: &brand, Last4: &last4, ExpMonth: &month, ExpYear: &year}}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "legacy-" + schedule, Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule,
			StartedAt: start, PaidThrough: &end, PaymentMethod: method}},
		Transactions: []billing.DeclaredTransaction{{RailSubscriptionID: schedule, TransactionID: paid.TransactionID, Success: true, Amount: 9_990_000, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(w.t, err)
	require.Len(w.t, result.Imported, 1, "%+v", result)
	subs, err := shop.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(w.t, err)
	for _, sub := range subs.Items {
		if sub.RailSubscriptionID != nil && *sub.RailSubscriptionID == schedule {
			return sub.ID
		}
	}
	w.t.Fatal("the imported schedule")
	return billing.SubscriptionID{}
}

// order is an open order the merchant's backend made for its customer.
func (w *world) order(mid billing.MerchantID, customer billing.CustomerID) billing.OrderID {
	w.t.Helper()
	ctx, shop := w.t.Context(), handle(w.t, w.srv, mid)
	key := "item-" + uuid.NewString()[:8]
	product, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Item " + key})
	require.NoError(w.t, err)
	price, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 5_000_000})
	require.NoError(w.t, err)
	o, err := shop.CreateOrder(ctx, billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: uuid.NewString()})
	require.NoError(w.t, err)
	return o.ID
}

// waitJob waits for a River job to complete, running it now when it waits
// for its time.
func (w *world) waitJob(id int64) {
	w.t.Helper()
	ctx := w.t.Context()
	graph, _ := operator.Of(w.srv)
	jobs := graph.Runtime.RiverProducer
	var last *rivertype.JobRow
	ok := assert.Eventually(w.t, func() bool {
		job, err := jobs.JobGet(ctx, id)
		if err != nil {
			return false
		}
		last = job
		if job.State == rivertype.JobStateScheduled || job.State == rivertype.JobStateRetryable {
			_, _ = jobs.JobRetry(ctx, id)
		}
		return job.State == rivertype.JobStateCompleted
	}, time.Minute, 50*time.Millisecond, "job %d", id)
	if !ok && last != nil {
		w.t.Fatalf("job %d %s %s: %v", last.ID, last.Kind, last.State, last.Errors)
	}
	require.True(w.t, ok)
}

// refresh runs the merchant's provider refresh, as a healthy deployment
// observes its PSPs.
func (w *world) refresh(mid billing.MerchantID) {
	w.t.Helper()
	graph, _ := operator.Of(w.srv)
	id, _, err := riverjobs.EnqueueMerchantRefresh(w.t.Context(), graph.Runtime.RiverProducer, mid.UUID(), openrails.QueueBilling)
	require.NoError(w.t, err)
	w.waitJob(id)
}

// due runs the engine's due pass at at: a provider refresh just before, as
// a healthy deployment observes its PSPs, then the pass and the work it
// queues. River runs jobs by the wall clock; the engine schedules them by
// the world's, so work due by it is promoted.
func (w *world) due(clock *clockwork.FakeClock, mid billing.MerchantID, at time.Time) {
	w.t.Helper()
	ctx := w.t.Context()
	graph, _ := operator.Of(w.srv)
	jobs := graph.Runtime.RiverProducer
	clock.Advance(at.Add(-time.Minute).Sub(clock.Now()))
	w.refresh(mid)
	clock.Advance(at.Sub(clock.Now()))
	pass, err := jobs.Insert(ctx, riverjobs.DunningArgs{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(pass.Job.ID)
	kinds := []string{"openrails.provider_operation", "openrails.subscription_converge", "openrails.card_refresh", "openrails.dunning", "openrails.invoice"}
	require.Eventually(w.t, func() bool {
		page, err := jobs.JobList(ctx, river.NewJobListParams().Kinds(kinds...).States(rivertype.JobStateScheduled).First(100))
		if err != nil {
			return false
		}
		for _, job := range page.Jobs {
			if !job.ScheduledAt.After(clock.Now()) {
				_, _ = jobs.JobRetry(ctx, job.ID)
			}
		}
		page, err = jobs.JobList(ctx, river.NewJobListParams().Kinds(kinds...).States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStatePending).First(50))
		return err == nil && len(page.Jobs) == 0
	}, time.Minute, 50*time.Millisecond, "the pass's work settles")
}

// The network portal's API: an openrails.dev account, linked at two
// merchants (and unlinked at a third), reads its links, subscriptions and
// orders across them, and acts through one link as that merchant's customer:
// adding a card and moving one subscription to it, canceling through the
// merchant's own operation (an NMI-run schedule while the merchant's
// destructive actions are disarmed answers provider_cancel_held), and
// revoking one charge agreement, which ends its subscription at the
// period's end and charges nothing more under it. Neither merchant learns
// anything of the other.
func TestNetworkPortal(t *testing.T) {
	// The engine's time runs in the past, so the periods it advances through
	// stay behind the database's own clock.
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-4 * 365 * 24 * time.Hour).Truncate(time.Second))
	w := newWorldAt(t, clock)
	ctx := t.Context()
	label := uuid.NewString()[:8]
	martLogin := newIssuerKey(t, "https://mart-"+label+".e2e.test")
	trust(t, w.srv, w.mart, martLogin.app(t, "viewer", nil))
	martClient, martKey := w.client(w.mart), merchantKey(t, w.srv, w.mart, "owner")

	ada := w.account(true)
	shopSub := uuid.NewString()
	shopToken := w.customer(w.shopLogin, shopSub, nil)
	shopPlan := w.subscribe(w.shop, shopToken)
	shopLink := w.connect(shopToken, w.shopClient, ada)
	martSub := uuid.NewString()
	martToken := w.customer(martLogin, martSub, nil)
	martPlan := w.subscribe(w.mart, martToken)
	martVault := w.martGateway.LastSale().Vault
	held := w.providerRun(w.mart, billing.CustomerID(uuid.MustParse(martSub)))
	martLink := w.connect(martToken, martClient, ada)
	rivalSub := uuid.NewString()
	rivalToken := w.customer(w.rivalLogin, rivalSub, nil)
	rivalLink := w.connect(rivalToken, w.rivalClient, ada)
	rivalOrder := w.order(w.rival, billing.CustomerID(uuid.MustParse(rivalSub)))
	shopOrder := w.order(w.shop, billing.CustomerID(uuid.MustParse(shopSub)))
	martOrder := w.order(w.mart, billing.CustomerID(uuid.MustParse(martSub)))
	// Another customer of the mart, never linked: the control the due pass
	// does charge.
	bobToken := w.customer(martLogin, uuid.NewString(), nil)
	w.subscribe(w.mart, bobToken)
	bobVault := w.martGateway.LastSale().Vault

	// Who may call: the account itself, signed in to openrails.dev, having
	// accepted the network terms; sensitive routes want a recent sign-in.
	session := "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), ada).AccessToken
	stale := "Bearer " + authtest.StaleSession(t, w.srv.AuthKit(), authtest.SignIn(t, w.srv.AuthKit(), ada).AccessToken)
	w.refused(w.network(http.MethodGet, "/links", "", nil), http.StatusUnauthorized, billing.CodeAuthenticationRequired, "no credential")
	w.refused(w.network(http.MethodGet, "/links", "Bearer "+authtest.SignIn(t, w.srv.AuthKit(), w.account(false)).AccessToken, nil),
		http.StatusForbidden, billing.CodeNetworkTermsRequired, "an account that never accepted the network terms")
	bound := w.signIn(ada, w.shopClient, "n-"+uuid.NewString(), resourceID, "openid", billing.ScopeSelf)
	w.refused(w.network(http.MethodGet, "/links", "Bearer "+bound.AccessToken, nil), http.StatusForbidden, billing.CodePermissionRequired,
		"a token a merchant's client holds acts at that merchant only")
	w.refused(w.network(http.MethodGet, "/links", shopToken, nil), http.StatusForbidden, billing.CodePermissionRequired, "a merchant's own login's customer")
	r := w.network(http.MethodGet, "/links", stale, nil)
	require.Equal(t, http.StatusOK, r.Code, "reads need no step-up: %s", r.Body.String())
	r = w.network(http.MethodDelete, "/links/"+rivalLink.ID.String(), stale, nil)
	w.refused(r, http.StatusUnauthorized, billing.CodeStepUpRequired, "a stale sign-in")
	require.Equal(t, `Bearer error="insufficient_user_authentication", max_age="900"`, r.Header().Get("WWW-Authenticate"), "RFC 9470")

	// Unlinking the rival: nothing charges there.
	r = w.network(http.MethodDelete, "/links/"+rivalLink.ID.String(), session, nil)
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())
	w.refused(w.network(http.MethodGet, "/links/"+rivalLink.ID.String()+"/payment-methods", session, nil), http.StatusNotFound, billing.CodeResourceNotFound,
		"an ended link")

	// Reads across the linked merchants, a page of one at a time.
	listed := networkList[billing.NetworkLink](w, "/links", session, 1)
	require.Len(t, listed, 2)
	links := map[billing.MerchantID]billing.NetworkLink{}
	for _, l := range listed {
		links[l.Link.MerchantID] = l
		require.Equal(t, ada.ID, l.Link.AccountID.String())
	}
	require.Equal(t, shopLink.ID, links[w.shop].Link.ID)
	require.Equal(t, "Network Shop", links[w.shop].Merchant.DisplayName)
	require.Equal(t, martLink.ID, links[w.mart].Link.ID)
	require.Equal(t, "Network Mart", links[w.mart].Merchant.DisplayName)

	all := networkList[billing.NetworkSubscription](w, "/subscriptions", session, 50)
	paged := networkList[billing.NetworkSubscription](w, "/subscriptions", session, 1)
	require.Equal(t, all, paged, "a page of one at a time reads the same list")
	through := map[billing.MerchantID]billing.CustomerLinkID{w.shop: shopLink.ID, w.mart: martLink.ID}
	at := map[billing.SubscriptionID]billing.MerchantID{}
	for _, s := range all {
		at[s.Subscription.ID] = s.MerchantID
		require.Equal(t, through[s.MerchantID], s.LinkID, "the link it acts through")
	}
	require.Equal(t, map[billing.SubscriptionID]billing.MerchantID{shopPlan: w.shop, martPlan: w.mart, held: w.mart}, at, "both merchants', nobody else's")
	orders := map[billing.OrderID]billing.MerchantID{}
	for _, o := range networkList[billing.NetworkOrder](w, "/orders", session, 2) {
		orders[o.Order.ID] = o.MerchantID
		require.Contains(t, []billing.MerchantID{w.shop, w.mart}, o.MerchantID)
		require.Equal(t, through[o.MerchantID], o.LinkID)
	}
	require.Equal(t, w.shop, orders[shopOrder])
	require.Equal(t, w.mart, orders[martOrder])
	require.NotContains(t, orders, rivalOrder, "the unlinked merchant's")

	// Through one link, as that merchant's customer.
	shop, mart := "/links/"+shopLink.ID.String(), "/links/"+martLink.ID.String()
	r = w.network(http.MethodGet, mart+"/subscriptions/"+martPlan.String(), session, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, billing.SubscriptionActive, decode[billing.Subscription](t, r).Status)
	w.refused(w.network(http.MethodGet, shop+"/subscriptions/"+martPlan.String(), session, nil), http.StatusNotFound, "subscription_not_found",
		"the mart's subscription is not the shop's")
	w.refused(w.network(http.MethodGet, "/links/"+billing.CustomerLinkID(uuid.New()).String()+"/payment-methods", session, nil), http.StatusNotFound,
		billing.CodeResourceNotFound, "a link the account does not hold")
	other := w.connect(w.customer(w.shopLogin, uuid.NewString(), nil), w.shopClient, w.account(true))
	w.refused(w.network(http.MethodGet, "/links/"+other.ID.String()+"/payment-methods", session, nil), http.StatusNotFound,
		billing.CodeResourceNotFound, "another account's link")

	// A card added at the shop, and one subscription moved to it.
	psps, err := handle(w.t, w.srv, w.shop).ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(t, err)
	card := map[string]any{"psp_id": psps.Items[0].ID, "token": w.gatewayOf(w.shop).Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454"}),
		"billing_details": map[string]any{"name": "Network Customer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}
	w.refused(w.network(http.MethodPost, shop+"/payment-methods", stale, card), http.StatusUnauthorized, billing.CodeStepUpRequired, "adding a card is sensitive")
	r = w.network(http.MethodPost, shop+"/payment-methods", session, card)
	require.Less(t, r.Code, 300, r.Body.String())
	added := decode[billing.PaymentMethod](t, r)
	r = w.network(http.MethodPut, shop+"/subscriptions/"+shopPlan.String()+"/payment-method", session, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &added.ID})
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	moved := decode[billing.Subscription](t, r)
	require.NotNil(t, moved.PaymentMethodID)
	require.Equal(t, added.ID, *moved.PaymentMethodID)
	untouched, err := handle(w.t, w.srv, w.mart).GetSubscription(ctx, martPlan)
	require.NoError(t, err)
	require.NotNil(t, untouched.PaymentMethodID)
	require.NotEqual(t, added.ID, *untouched.PaymentMethodID, "the mart's subscription keeps its card")

	// Canceling goes through each merchant's own operation.
	cancel := func() billing.Subscription {
		t.Helper()
		r := w.network(http.MethodPost, shop+"/subscriptions/"+shopPlan.String()+"/cancel", session, map[string]any{"reason": "moving on"})
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		return decode[billing.Subscription](t, r)
	}
	canceled := cancel()
	require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
	require.True(t, canceled.CancelScheduled, "at the period's end, the shop's policy")
	require.True(t, canceled.CurrentPeriodEndsAt.Equal(*moved.CurrentPeriodEndsAt), "the paid period stands")
	r = w.network(http.MethodPost, shop+"/subscriptions/"+shopPlan.String()+"/resume", session, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	require.Equal(t, billing.SubscriptionActive, decode[billing.Subscription](t, r).Status, "resumed")
	require.True(t, cancel().CancelScheduled)
	w.refused(w.network(http.MethodPost, mart+"/subscriptions/"+held.String()+"/cancel", session, map[string]any{"reason": "moving on"}),
		http.StatusConflict, billing.CodeProviderCancelHeld, "NMI bills the schedule and the mart's destructive actions are disarmed")

	w.refresh(w.mart)
	// Each charge agreement is revoked on its own: the subscription's ends
	// the subscription at its period's end; a provider-billed schedule that
	// cannot be canceled (destructive actions disarmed) keeps its agreement.
	agreements := func() map[billing.MandateID]billing.Agreement {
		t.Helper()
		out := map[billing.MandateID]billing.Agreement{}
		for _, a := range networkList[billing.Agreement](w, mart+"/agreements", session, 1) {
			out[a.ID] = a
		}
		return out
	}
	byPlan := map[billing.SubscriptionID]billing.Agreement{}
	for _, a := range agreements() {
		require.Equal(t, billing.MandateRecurring, a.Kind, "the mart's agreements are its subscriptions'")
		require.NotNil(t, a.Subscription)
		byPlan[a.Subscription.ID] = a
	}
	planAgreement, heldAgreement := byPlan[martPlan], byPlan[held]
	require.False(t, planAgreement.ID.IsZero(), "the plan's agreement")
	require.False(t, heldAgreement.ID.IsZero(), "the NMI schedule's agreement")
	require.Equal(t, "USD", planAgreement.Currency)
	require.EqualValues(t, 9_990_000, planAgreement.Subscription.Amount)
	require.NotNil(t, planAgreement.Subscription.NextChargeAt, "it renews")
	require.NotNil(t, planAgreement.Card)
	require.Equal(t, untouched.PaymentMethodID, planAgreement.PaymentMethodID)

	revoke := func(id billing.MandateID, authorization string) *httptest.ResponseRecorder {
		return w.network(http.MethodPost, mart+"/agreements/"+id.String()+"/revoke", authorization, nil)
	}
	w.refused(revoke(planAgreement.ID, stale), http.StatusUnauthorized, billing.CodeStepUpRequired, "revoking is sensitive")
	w.refused(revoke(heldAgreement.ID, session), http.StatusConflict, billing.CodeProviderCancelHeld, "NMI keeps billing while the mart is disarmed")
	require.Contains(t, agreements(), heldAgreement.ID, "nothing revoked")
	r = revoke(planAgreement.ID, session)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	revocation := decode[billing.AgreementRevocation](t, r)
	require.Equal(t, billing.MandateRevoked, revocation.Agreement.Status)
	require.NotNil(t, revocation.Agreement.EndedAt)
	require.Nil(t, revocation.UsageOrder)
	require.NotNil(t, revocation.Subscription)
	require.Equal(t, martPlan, revocation.Subscription.ID)
	require.True(t, revocation.Subscription.CancelScheduled, "canceled at the period's end")
	require.NotContains(t, agreements(), planAgreement.ID)
	w.refused(revoke(planAgreement.ID, session), http.StatusNotFound, billing.CodeResourceNotFound, "revoked once")
	var event *billing.MandateHostEvent
	for _, e := range w.events(martKey, billing.HostEventMandateRevoked) {
		event = e.Mandate
	}
	require.NotNil(t, event, "mandate.revoked")
	require.Equal(t, billing.MandateHostEvent{MandateID: planAgreement.ID, Kind: billing.MandateRecurring, CustomerID: billing.CustomerID(uuid.MustParse(martSub)),
		Currency: "USD", SubscriptionID: &martPlan, RevokedAt: event.RevokedAt}, *event)
	require.Len(t, w.events(martKey, billing.HostEventMandateRevoked), 1, "once")
	require.Empty(t, w.events(w.shopKey, billing.HostEventMandateRevoked), "the shop hears nothing")
	var revokedMandate *billing.Mandate
	r = call(t, w.handler, martKey.Secret, http.MethodGet, "/v1/admin/customers/"+martSub+"/payment-methods", nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	for _, pm := range decode[billing.ListPage[billing.PaymentMethod]](t, r).Items {
		for _, m := range pm.Mandates {
			if m.ID == planAgreement.ID {
				revokedMandate = &m
			}
		}
	}
	require.NotNil(t, revokedMandate, "the merchant keeps the agreement as evidence")
	require.Equal(t, billing.MandateRevoked, revokedMandate.Status)
	require.Equal(t, billing.MandateEndCustomerRevoked, *revokedMandate.EndReason)
	w.refused(w.network(http.MethodDelete, "/links/"+martLink.ID.String(), session, nil), http.StatusConflict, billing.CodeNetworkLinkInUse,
		"the NMI schedule still charges")

	// At the period's end nothing is charged under the revoked agreement and
	// the subscription ends; the mart's other customer renews.
	plan, err := handle(w.t, w.srv, w.mart).GetSubscription(ctx, martPlan)
	require.NoError(t, err)
	require.NotNil(t, plan.CurrentPeriodEndsAt)
	end := *plan.CurrentPeriodEndsAt
	w.due(clock, w.mart, end.Add(time.Second))
	require.Eventually(t, func() bool { return len(w.martGateway.Ledger(bobVault)) == 2 }, 30*time.Second, 100*time.Millisecond,
		"the pass charges a renewal under an active agreement")
	require.Len(t, w.martGateway.Ledger(martVault), 1, "nothing under the revoked agreement")
	plan, err = handle(w.t, w.srv, w.mart).GetSubscription(ctx, martPlan)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionCanceled, plan.Status)
	require.False(t, plan.CancelScheduled, "ended")
	require.True(t, plan.CurrentPeriodEndsAt.Equal(end), "no period after it")

	// Neither merchant learns of the other.
	for _, side := range []struct {
		key           iam.APIKeyCreated
		own, other    string
		otherMerchant billing.MerchantID
		otherLink     billing.CustomerLinkID
		otherPlan     billing.SubscriptionID
		otherOrder    billing.OrderID
	}{
		{w.shopKey, shopSub, martSub, w.mart, martLink.ID, martPlan, martOrder},
		{martKey, martSub, shopSub, w.shop, shopLink.ID, shopPlan, shopOrder},
	} {
		r := call(t, w.handler, side.key.Secret, http.MethodGet, "/v1/admin/customers/"+side.own, nil)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		for _, other := range []string{side.other, side.otherMerchant.String(), side.otherLink.String()} {
			require.NotContains(t, r.Body.String(), other, "a merchant's read of its customer")
		}
		for _, path := range []string{"/v1/admin/subscriptions/" + side.otherPlan.String(), "/v1/admin/orders/" + side.otherOrder.String(), "/v1/admin/customers/" + side.other} {
			r := call(t, w.handler, side.key.Secret, http.MethodGet, path, nil)
			require.Equal(t, http.StatusNotFound, r.Code, "%s: %s", path, r.Body.String())
		}
	}

	// The network portal belongs to an account bound to no merchant: a
	// credential bound to one, a merchant's key or its customer's token, is
	// refused there.
	boundToken := martLogin.mint(t, func(c jwt.MapClaims) {
		c["sub"], c["scope"] = uuid.NewString(), billing.ScopeSelf
		delete(c, "permissions")
	})
	for name, token := range map[string]string{"a merchant's key": martKey.Secret, "a merchant's customer token": boundToken} {
		r := serve(w.handler, rsRequest{path: "/v1/network/links", authorization: "Bearer " + token})
		require.Equal(t, http.StatusForbidden, r.Code, "%s: %s", name, r.Body.String())
	}
}
