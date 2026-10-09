//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// Hosted checkout (#1124): several apps share one merchant and one billing
// database; one of them also serves the payment page. A signed-in customer
// mints a session on an app, and its id alone reads and pays it on the
// payment host.

const (
	hostedAppOrigin   = "https://host-two.example"
	hostedOtherOrigin = "https://host-three.example"
	hostedPageURL     = "https://pay.example/checkout"
)

// hostedHosts starts two host processes on one merchant and database: app
// mints sessions, pay serves the payment page's routes. Each has its own
// connections, River client and HTTP server; they share the engine clock.
func hostedHosts(t *testing.T, deps func(*openrails.Deps)) (app, pay *world) {
	t.Helper()
	fleetSlots <- struct{}{}
	t.Cleanup(func() { <-fleetSlots })
	deadline(t, fleetDeadline)
	f := &fleet{t: t, base: prepareWorld(t, 4), dead: map[*world]bool{}}
	process := func(name string, origins []string, checkout openrails.CheckoutConfig) *world {
		b := f.base
		w := &world{t: t, pool: b.pool, dsn: b.dsn, schema: b.schema, slug: b.slug, stripe: b.stripe, nmi: b.nmi, auth: b.auth, clock: b.clock,
			deps: deps, replica: &replicaEnv{f: f, name: name, queue: "replica_" + name},
			cfg: func(c *config.Config) {
				c.ReturnOrigins, c.PublicBillingBaseURL, c.Checkout = origins, origins[0]+"/billing", checkout
			},
		}
		w.start()
		t.Cleanup(w.stop)
		f.replicas = append(f.replicas, w)
		return w
	}
	app = process("a", []string{hostedAppOrigin, hostedOtherOrigin}, openrails.CheckoutConfig{PageURL: hostedPageURL})
	pay = process("b", []string{"https://host-one.example"}, openrails.CheckoutConfig{PageURL: hostedPageURL, EmbedOrigins: []string{hostedAppOrigin}})
	return app, pay
}

var hostedAddress atomic.Int64

// guest calls a billing route as the payment page does: no credential. Each
// call comes from its own address, so only the per-session limits apply.
func (w *world) guest(method, path string, body any) (int, map[string]any) {
	w.t.Helper()
	return w.page(method, path, "", body)
}

// page calls a billing route from the payment page, with token when the
// customer is signed in there.
func (w *world) page(method, path, token string, body any) (int, map[string]any) {
	w.t.Helper()
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.server.URL+mountPrefix+path, data)
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", hostedAddress.Add(1)%250+1))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	out := map[string]any{}
	if len(raw) > 0 {
		require.NoError(w.t, json.Unmarshal(raw, &out), "%s %s: %s", method, path, raw)
	}
	return res.StatusCode, out
}

func hostedErrorCode(body map[string]any) string {
	detail, _ := body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}

// mint is the signed-in customer starting checkout on their app.
func (c *customer) mint(body map[string]any) map[string]any {
	c.w.t.Helper()
	status, out := c.call(http.MethodPost, "/checkout-sessions", "", body)
	require.Equal(c.w.t, http.StatusCreated, status, "%v", out)
	require.True(c.w.t, strings.HasPrefix(out["id"].(string), "ocs_"), "%v", out)
	require.Len(c.w.t, out["id"], len("ocs_")+64)
	return out
}

// hostedSession is one minted session as the payment page drives it.
type hostedSession struct {
	w  *world
	id string
}

// session is the signed-in customer's own session, as the page drives it.
func (c *customer) session(body map[string]any) hostedSession {
	c.w.t.Helper()
	return hostedSession{w: c.w, id: c.mint(body)["id"].(string)}
}

// handOver is the merchant handing a purchase to its customer: a session
// minted server-side through the Client.
func (w *world) handOver(c *customer, price billing.PriceID) hostedSession {
	w.t.Helper()
	link, err := w.client[remote].CreateCheckoutSession(w.t.Context(), billing.CreateCheckoutSessionParams{Customer: c.identity(), PriceID: price})
	require.NoError(w.t, err)
	return hostedSession{w: w, id: link.ID}
}

func (s hostedSession) read() map[string]any {
	s.w.t.Helper()
	status, doc := s.w.guest(http.MethodGet, "/v1/checkout-sessions/"+s.id, nil)
	require.Equal(s.w.t, http.StatusOK, status, "%v", doc)
	return doc
}

// readAs is the session as c sees it signed in: with c's saved cards when c
// is its customer.
func (s hostedSession) readAs(c *customer) map[string]any {
	s.w.t.Helper()
	status, doc := s.w.page(http.MethodGet, "/v1/checkout-sessions/"+s.id, c.token, nil)
	require.Equal(s.w.t, http.StatusOK, status, "%v", doc)
	return doc
}

// option is the session's option handle for rail.
func (s hostedSession) option(rail string) string {
	s.w.t.Helper()
	for _, raw := range s.read()["options"].([]any) {
		if option := raw.(map[string]any); option["rail"] == rail {
			require.NotContains(s.w.t, option, "selector", "the engine selector stays on the server")
			return option["id"].(string)
		}
	}
	s.w.t.Fatalf("no %s option", rail)
	return ""
}

func (s hostedSession) pay(body map[string]any) (int, map[string]any) {
	s.w.t.Helper()
	return s.w.guest(http.MethodPost, "/v1/checkout-sessions/"+s.id+"/pay", body)
}

// payAs is c paying the session signed in.
func (s hostedSession) payAs(c *customer, body map[string]any) (int, map[string]any) {
	s.w.t.Helper()
	return s.w.page(http.MethodPost, "/v1/checkout-sessions/"+s.id+"/pay", c.token, body)
}

// payWithSaved is the customer paying s, signed in, with a card they saved
// on rail, and the membership it bought.
func (c *customer) payWithSaved(s hostedSession, rail, method string) billing.SubscriptionID {
	c.w.t.Helper()
	status, out := s.payAs(c, map[string]any{"option_id": s.option(rail), "payment_method_id": method})
	require.Equal(c.w.t, http.StatusOK, status, "%v", out)
	require.Equal(c.w.t, "succeeded", out["status"], "%v", out)
	c.w.settle()
	id, err := billing.ParseSubscriptionID(fmt.Sprint(out["subscription_id"]))
	require.NoError(c.w.t, err, "the payment names the membership: %v", out)
	return id
}

// payCard pays with a card the page just tokenized.
func (s hostedSession) payCard(c card) (int, map[string]any) {
	s.w.t.Helper()
	return s.pay(map[string]any{"option_id": s.option("nmi"), "payment_token": s.w.nmi.Tokenize(c), "billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}})
}

// attempt is the session's stored payment attempt.
func (s hostedSession) attempt() int {
	s.w.t.Helper()
	var n int
	require.NoError(s.w.t, s.w.pool.QueryRow(s.w.t.Context(), s.w.q(`SELECT attempt FROM billing.checkout_sessions WHERE id_hash = sha256($1::bytea)`), []byte(s.id)).Scan(&n))
	return n
}

// sessionIDs fails a test when any log line carries a session id.
var sessionIDs = struct {
	once   sync.Once
	mu     sync.Mutex
	leaked []string
}{}

type sessionIDLogHook struct{}

var sessionIDPattern = regexp.MustCompile(`ocs_[0-9a-f]{64}`)

func (sessionIDLogHook) Levels() []log.Level { return log.AllLevels }
func (sessionIDLogHook) Fire(e *log.Entry) error {
	line, err := e.String()
	if err != nil {
		line = e.Message
	}
	if id := sessionIDPattern.FindString(line); id != "" {
		sessionIDs.mu.Lock()
		sessionIDs.leaked = append(sessionIDs.leaked, line)
		sessionIDs.mu.Unlock()
	}
	return nil
}

func watchSessionIDLogs(t *testing.T) {
	sessionIDs.once.Do(func() { log.AddHook(sessionIDLogHook{}) })
	t.Cleanup(func() {
		sessionIDs.mu.Lock()
		defer sessionIDs.mu.Unlock()
		require.Empty(t, sessionIDs.leaked, "a session id is a credential and is never logged")
	})
}

// Mint on one host process, read and pay on another against the same
// database; a replayed pay charges once.
func TestHostedCheckoutAcrossHosts(t *testing.T) {
	t.Parallel()
	watchSessionIDLogs(t)
	app, pay := hostedHosts(t, nil)
	buyer := app.newCustomer()
	price := app.membership("content:members", 9_990_000)
	product, err := app.client[embedded].GetProduct(t.Context(), price.ProductID)
	require.NoError(t, err)

	minted := buyer.mint(map[string]any{"product_key": product.Key, "price_key": price.Key, "success_url": hostedAppOrigin + "/welcome"})
	session := hostedSession{w: pay, id: minted["id"].(string)}
	require.Equal(t, hostedPageURL+"#"+session.id, minted["url"], "the app frames the shared page")

	doc := session.read()
	require.Equal(t, "created", doc["status"])
	require.Equal(t, hostedAppOrigin, doc["embed_origin"], "the page talks only to the app that minted the session")
	require.Equal(t, hostedAppOrigin+"/welcome", doc["success_url"])
	require.Equal(t, map[string]any{"auto_renew": true, "display_name": "Membership", "unit_amount": "9990000", "currency": "USD", "unit_decimals": float64(6), "billing_interval_hours": float64(monthHours), "access_duration_hours": float64(monthHours)}, doc["plan"])
	require.Equal(t, "9990000", doc["due_today"])
	require.Equal(t, map[string]any{"display_name": app.slug}, doc["merchant"])

	// A double click and a retry are one attempt: one idempotency key, one charge.
	body := map[string]any{"option_id": session.option("nmi"), "payment_token": pay.nmi.Tokenize(visa), "billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	subscriptions := map[string]bool{}
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, out := session.pay(body)
			mu.Lock()
			defer mu.Unlock()
			if status == http.StatusConflict {
				require.Equal(t, "checkout_payment_in_progress", hostedErrorCode(out), "%v", out)
				return
			}
			require.Equal(t, http.StatusOK, status, "%v", out)
			require.Equal(t, "succeeded", out["status"], "%v", out)
			subscriptions[out["subscription_id"].(string)] = true
		}()
	}
	wg.Wait()
	status, replay := session.pay(body)
	require.Equal(t, http.StatusOK, status, "%v", replay)
	require.Equal(t, "succeeded", replay["status"])
	subscriptions[replay["subscription_id"].(string)] = true
	require.Len(t, subscriptions, 1, "every submission answers with the one membership")
	pay.settle()
	require.Len(t, pay.nmi.ledger(""), 1, "one charge")
	require.Equal(t, 0, session.attempt())

	// The app confirms from its own authenticated API before granting access.
	require.True(t, buyer.entitled("content:members"))
	subs := unwrap(buyer.must(http.MethodGet, "/subscriptions", "", nil))
	require.Len(t, subs["data"], 1, "%v", subs)
	require.Equal(t, "succeeded", session.read()["status"])

	// Only the hash is stored.
	var row string
	require.NoError(t, pay.pool.QueryRow(t.Context(), pay.q(`SELECT row_to_json(s)::text FROM billing.checkout_sessions s`)).Scan(&row))
	require.NotContains(t, row, session.id)
	require.NotContains(t, row, session.id[len("ocs_"):])
	require.Empty(t, pay.nmi.Unexpected())
}

// The attempt advances only after a terminal failure, and then a new card
// pays under a new key.
func TestHostedCheckoutDeclineThenRetry(t *testing.T) {
	t.Parallel()
	app, pay := hostedHosts(t, nil)
	buyer := app.newCustomer()
	price := app.membership("content:members", 9_990_000)
	session := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}

	status, declined := session.payCard(card{Brand: "visa", Last4: "0002", Decline: "202"})
	require.Equal(t, http.StatusOK, status, "%v", declined)
	require.Equal(t, "failed", declined["status"])
	failure := declined["failure"].(map[string]any)
	require.NotEmpty(t, failure["reason"])
	require.Equal(t, failure["message"], declined["failure_message"])
	require.Equal(t, 1, session.attempt(), "the declined attempt is over")
	require.Equal(t, "created", session.read()["status"], "the session takes another card")
	require.Empty(t, pay.nmi.ledger(""))

	status, paid := session.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", paid)
	require.Equal(t, "succeeded", paid["status"])
	pay.settle()
	require.Len(t, pay.nmi.ledger(""), 1)
	require.Equal(t, 1, session.attempt(), "a paid attempt stays current")
	require.True(t, buyer.entitled("content:members"))

	// A member cannot buy the membership twice: the purchase itself is refused.
	again := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}
	status, refused := again.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", refused)
	require.Equal(t, "blocked", refused["status"], "%v", refused)
	require.NotEmpty(t, refused["failure_message"])
	require.Len(t, pay.nmi.ledger(""), 1)

	// The last attempt ends the session instead of advancing.
	buyer = app.newCustomer()
	last := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}
	_, err := pay.pool.Exec(t.Context(), pay.q(`UPDATE billing.checkout_sessions SET attempt = 9 WHERE id_hash = sha256($1::bytea)`), []byte(last.id))
	require.NoError(t, err)
	status, over := last.payCard(card{Brand: "visa", Last4: "0002", Decline: "202"})
	require.Equal(t, http.StatusOK, status, "%v", over)
	require.Equal(t, "expired", over["status"], "%v", over)
	require.Equal(t, 10, last.attempt())
	require.Equal(t, "expired", last.read()["status"])
	status, over = last.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", over)
	require.Equal(t, "expired", over["status"], "a spent session charges nothing")
	require.Len(t, pay.nmi.ledger(""), 1)
}

// An expired or unknown id is refused, and expired rows are swept.
func TestHostedCheckoutExpiryAndUnknownIDs(t *testing.T) {
	t.Parallel()
	app, pay := hostedHosts(t, nil)
	buyer := app.newCustomer()
	price := app.membership("content:members", 9_990_000)
	body := func(s hostedSession) map[string]any {
		return map[string]any{"option_id": s.option("nmi"), "payment_token": pay.nmi.Tokenize(visa), "billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}
	}

	for _, id := range []string{"ocs_" + strings.Repeat("0", 64), "ocs_short", uuid.NewString()} {
		status, out := pay.guest(http.MethodGet, "/v1/checkout-sessions/"+id, nil)
		require.Equal(t, http.StatusNotFound, status, "%s: %v", id, out)
		status, out = pay.guest(http.MethodPost, "/v1/checkout-sessions/"+id+"/pay", map[string]any{"option_id": "option_x"})
		require.Equal(t, http.StatusNotFound, status, "%s: %v", id, out)
	}

	minted := buyer.mint(map[string]any{"price_id": price.ID})
	session := hostedSession{w: pay, id: minted["id"].(string)}
	expires, err := time.Parse(time.RFC3339Nano, minted["expires_at"].(string))
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, expires.Sub(app.clock.Now()))
	pay1 := body(session)

	app.advance(30*time.Minute + time.Second)
	status, out := session.pay(pay1)
	require.Equal(t, http.StatusGone, status, "%v", out)
	require.Equal(t, "checkout_session_expired", hostedErrorCode(out))
	require.Equal(t, "expired", session.read()["status"], "an expired session still reads until its reconciliation window ends")
	require.Empty(t, pay.nmi.ledger(""))

	// Past the reconciliation window the id is unknown and its row is swept.
	app.advance(24 * time.Hour)
	status, out = pay.guest(http.MethodGet, "/v1/checkout-sessions/"+session.id, nil)
	require.Equal(t, http.StatusNotFound, status, "%v", out)
	live := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}
	res, err := pay.jobs.Insert(t.Context(), cleanupPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	pay.waitJob(res.Job.ID)
	var rows int
	require.NoError(t, pay.pool.QueryRow(t.Context(), pay.q(`SELECT count(*) FROM billing.checkout_sessions`)).Scan(&rows))
	require.Equal(t, 1, rows, "the sweep deletes only sessions past their window")
	require.Equal(t, "created", live.read()["status"])
}

// A site outside EmbedOrigins cannot frame the page, and the page never
// messages it.
func TestHostedCheckoutEmbedOrigins(t *testing.T) {
	t.Parallel()
	app, pay := hostedHosts(t, nil)
	buyer := app.newCustomer()
	price := app.membership("content:members", 9_990_000)

	page := func(w *world) string {
		rec := httptest.NewRecorder()
		openrailshttp.CheckoutFramePolicy(w.rt)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/checkout", nil))
		return rec.Header().Get("Content-Security-Policy")
	}
	require.Equal(t, "frame-ancestors 'self' "+hostedAppOrigin, page(pay), "only the payment host and the listed sites may frame the page")
	require.Equal(t, "frame-ancestors 'self'", page(app), "an app that serves no payment page lists nobody")

	listed := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}
	require.Equal(t, hostedAppOrigin, listed.read()["embed_origin"], "the app's origin comes from its configuration")

	// The app also answers on an origin the payment host does not list.
	unlisted := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID, "success_url": hostedOtherOrigin + "/welcome"})["id"].(string)}
	require.Nil(t, unlisted.read()["embed_origin"])

	// A return URL is one of the app's own origins.
	status, out := buyer.call(http.MethodPost, "/checkout-sessions", "", map[string]any{"price_id": price.ID, "success_url": "https://elsewhere.example/welcome"})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
}

// Read and pay check the customer's standing, as the host last pushed it, on
// every action: a blocked customer neither reads, pays (a new card or a
// saved one) nor mints.
func TestHostedCheckoutAccountState(t *testing.T) {
	t.Parallel()
	app, pay := hostedHosts(t, nil)
	buyer := app.newCustomer()
	method := buyer.saveCard("nmi", visa)
	price := app.membership("content:members", 9_990_000)
	session := hostedSession{w: pay, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}
	minted := app.handOver(buyer, price.ID)
	option, mintedOption := session.option("nmi"), minted.option("nmi")
	block := func(blocked bool) {
		_, err := app.client[remote].EnsureCustomers(t.Context(), []billing.EnsureCustomerParams{{ID: buyer.cid(), Blocked: blocked}})
		require.NoError(t, err)
	}

	block(true)
	status, out := pay.guest(http.MethodGet, "/v1/checkout-sessions/"+session.id, nil)
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	require.Equal(t, "customer_blocked", hostedErrorCode(out))
	status, out = session.pay(map[string]any{"option_id": option, "payment_token": pay.nmi.Tokenize(visa), "billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}})
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	require.Equal(t, "customer_blocked", hostedErrorCode(out))
	status, out = minted.payAs(buyer, map[string]any{"option_id": mintedOption, "payment_method_id": method})
	require.Equal(t, http.StatusForbidden, status, "a session minted before the block: %v", out)
	require.Equal(t, "customer_blocked", hostedErrorCode(out))
	status, out = buyer.call(http.MethodPost, "/checkout-sessions", "", map[string]any{"price_id": price.ID})
	require.Equal(t, http.StatusForbidden, status, "%v", out)
	require.Equal(t, "customer_blocked", hostedErrorCode(out))
	_, err := app.client[remote].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{Customer: buyer.identity(), PriceID: price.ID})
	require.Error(t, err, "the host cannot mint for a blocked customer either")
	pay.settle()
	require.Empty(t, pay.nmi.ledger(""))
	require.False(t, buyer.entitled("content:members"))

	block(false)
	status, out = session.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "succeeded", out["status"])
}

// A host that starts checkout server-side mints through the Client; the buyer
// pays with a card they already saved.
func TestHostedCheckoutClientMintAndSavedCard(t *testing.T) {
	t.Parallel()
	app, pay := hostedHosts(t, nil)
	price := app.membership("content:members", 9_990_000)
	product, err := app.client[embedded].GetProduct(t.Context(), price.ProductID)
	require.NoError(t, err)
	for _, tp := range []topology{embedded, remote} {
		buyer := app.newCustomer()
		method := buyer.saveCard("nmi", visa)
		link, err := app.client[tp].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{
			Customer: billing.CheckoutCustomerIdentity{ID: cid(buyer.id)}, ProductKey: product.Key, PriceKey: price.Key,
		})
		require.NoError(t, err, tp)
		require.Equal(t, hostedPageURL+"#"+link.ID, *link.URL)
		session := hostedSession{w: pay, id: link.ID}

		require.Empty(t, session.read()["saved_methods"], "a guest sees no saved cards")
		saved := session.readAs(buyer)["saved_methods"].([]any)
		require.Len(t, saved, 1, "the buyer's saved cards, display data only")
		card := saved[0].(map[string]any)
		shown, _ := card["card"].(map[string]any)
		require.Equal(t, []any{method, session.option("nmi"), "nmi", "4242"}, []any{card["id"], card["option_id"], card["rail"], shown["last4"]})

		other := app.newCustomer().saveCard("nmi", mastercard)
		status, out := session.payAs(buyer, map[string]any{"option_id": session.option("nmi"), "payment_method_id": other})
		require.Equal(t, http.StatusUnprocessableEntity, status, "another customer's card: %v", out)
		status, out = session.pay(map[string]any{"option_id": session.option("nmi"), "payment_method_id": method})
		require.Equal(t, http.StatusForbidden, status, "a saved card needs its customer signed in: %v", out)
		require.Equal(t, "customer_proof_required", hostedErrorCode(out))

		status, out = session.payAs(buyer, map[string]any{"option_id": session.option("nmi"), "payment_method_id": method})
		require.Equal(t, http.StatusOK, status, "%v", out)
		require.Equal(t, "succeeded", out["status"])
		pay.settle()
		require.True(t, buyer.entitled("content:members"))
	}

	_, err = app.client[embedded].CreateCheckoutSession(t.Context(), billing.CreateCheckoutSessionParams{
		Customer: billing.CheckoutCustomerIdentity{ID: cid(uuid.NewString())}, ProductKey: product.Key, PriceKey: "no-such-price",
	})
	require.ErrorIs(t, err, billing.ErrInvalid)
}

// With no payment page configured the app renders checkout itself against
// the same three routes.
func TestHostedCheckoutSingleSite(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	buyer := w.newCustomer()
	product, err := w.client[embedded].CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
	require.NoError(t, err)
	price, err := w.client[embedded].CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)

	minted := buyer.mint(map[string]any{"product_key": product.Key, "price_key": price.Key})
	require.Nil(t, minted["url"], "no payment page: the app renders <Checkout> itself")
	session := hostedSession{w: w, id: minted["id"].(string)}
	doc := session.read()
	require.Nil(t, doc["embed_origin"])
	require.Nil(t, doc["plan"].(map[string]any)["billing_interval_hours"])

	status, out := session.payCard(visa)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "succeeded", out["status"])
	w.settle()
	require.True(t, buyer.entitled("content:post"))
	require.Len(t, w.nmi.ledger(""), 1)

	// Minting needs the signed-in customer.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/me/checkout-sessions", strings.NewReader(`{"product_key":"`+product.Key+`","price_key":"`+price.Key+`"}`))
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
}

// A PSP whose card_entry is server (#1129) takes the card on OpenRails through
// the shared page too: the page posts it with the session id, OpenRails vaults
// it once and charges, and a replay charges nothing more.
func TestHostedCheckoutServerCardEntry(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		account := psps["nmi"]
		account.Settings = map[string]any{"card_entry": "server"}
		psps["nmi"] = account
	}
	w.start()
	buyer := w.newCustomer()
	price := w.membership("content:members", 9_990_000)
	session := hostedSession{w: w, id: buyer.mint(map[string]any{"price_id": price.ID})["id"].(string)}

	var option string
	for _, raw := range session.read()["options"].([]any) {
		if rail := raw.(map[string]any); rail["rail"] == "nmi" {
			require.Equal(t, "card", rail["driver"], "%v", rail)
			require.Nil(t, rail["public_config"], "no gateway key: the page loads no gateway script")
			option = rail["id"].(string)
		}
	}
	require.NotEmpty(t, option)
	body := func() map[string]any {
		return map[string]any{"option_id": option, "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}
	}

	withToken := body()
	withToken["payment_token"] = "tok-1234"
	status, out := session.pay(withToken)
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.Empty(t, w.cardVaults("add_customer"))

	status, paid := session.pay(body())
	require.Equal(t, http.StatusOK, status, "%v", paid)
	require.Equal(t, "succeeded", paid["status"], "%v", paid)
	w.settle()
	require.True(t, buyer.entitled("content:members"))
	require.Len(t, w.nmi.ledger(""), 1)
	vaults := w.cardVaults("add_customer")
	require.Len(t, vaults, 1, "the card was vaulted once")
	require.Equal(t, entryVisa, vaults[0].Get("ccnumber"))

	status, again := session.pay(body())
	require.Equal(t, http.StatusOK, status, "%v", again)
	require.Equal(t, "succeeded", again["status"])
	require.Equal(t, paid["subscription_id"], again["subscription_id"])
	require.Len(t, w.nmi.ledger(""), 1, "a replay charges nothing more")
	require.Len(t, w.cardVaults("add_customer"), 1, "nor sends the card again")

	saved := session.readAs(buyer)["saved_methods"].([]any)
	require.Len(t, saved, 1, "the vaulted card is offered on the card option")
	require.Equal(t, option, saved[0].(map[string]any)["option_id"])
}

// A Stripe merchant with Elements sells a subscription on the session: the
// buyer saves a card in Stripe's own fields (the app's billing client) and
// the session pays with it; an issuer challenge is the session's operation to
// authenticate.
func TestCheckoutSessionPaysWithAStripeElementsCard(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		account := psps["stripe"]
		account.Settings = map[string]any{"publishable_key": "pk_test_e2e"}
		psps["stripe"] = account
	}
	w.start()
	price := w.membership("content:members", 9_990_000)
	option := w.options(billing.CheckoutOptionListParams{PriceID: price.ID})["stripe"]
	require.Equal(t, "stripe_elements", option.Driver)
	require.Equal(t, "pk_test_e2e", option.PublicConfig["publishable_key"])

	c := w.newCustomer()
	method := c.saveCard("stripe", visa)
	session := w.handOver(c, price.ID)
	saved := session.readAs(c)["saved_methods"].([]any)
	require.Len(t, saved, 1, "%v", saved)
	require.Equal(t, method, saved[0].(map[string]any)["id"])
	status, out := session.pay(map[string]any{"option_id": session.option("stripe"), "payment_token": "tok_x"})
	require.Equal(t, http.StatusUnprocessableEntity, status, "the page enters no Stripe card itself: %v", out)
	status, out = session.payAs(c, map[string]any{"option_id": session.option("stripe"), "payment_method_id": method})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "succeeded", out["status"], "%v", out)
	w.settle()
	require.True(t, c.entitled("content:members"))

	challenged := w.newCustomer()
	challenge := challenged.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	pending := w.handOver(challenged, price.ID)
	status, out = pending.payAs(challenged, map[string]any{"option_id": pending.option("stripe"), "payment_method_id": challenge})
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "requires_action", out["status"], "%v", out)
	operation := out["operation"].(map[string]any)
	require.NotEmpty(t, operation["id"])
	require.Equal(t, operation["id"], pending.read()["operation"].(map[string]any)["id"], "a reload resumes the authentication")
	require.False(t, challenged.entitled("content:members"))
}
