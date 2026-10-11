//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	stdlog "log"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Server card entry (#1129): a PSP declared card_entry: server takes the card
// on OpenRails itself and vaults it at the gateway. NMI's documented sandbox
// test cards; the security code is chosen so a scan for it has no innocent
// match (a leading zero is never a number, a UUID group or a timestamp).
const (
	entryVisa       = "4111111111111111"
	entryMastercard = "5431111111111111"
	entryCVC        = "0739"
)

func entryCard(number string) map[string]any {
	return map[string]any{"number": number, "exp_month": 10, "exp_year": 2027, "cvc": entryCVC}
}

// serverEntryWorld is a merchant whose NMI PSP takes cards on its own server.
func serverEntryWorld(t *testing.T) *world {
	t.Helper()
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) {
		account := psps["nmi"]
		account.Settings = map[string]any{"card_entry": "server"}
		psps["nmi"] = account
	}
	w.start()
	return w
}

// relayPay is a purchase with a card entered on OpenRails: the customer
// saves the card, which vaults it at the gateway, and pays the merchant's
// order with it. A refused save answers its status and body, and no order.
func (w *world) relayPay(c *customer, price billing.PriceID, card map[string]any) (int, map[string]any, *paidOrder) {
	w.t.Helper()
	status, saved := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": card,
		"billing_details": map[string]any{"name": "Hosted Payer", "address": map[string]any{"postal_code": "10001", "country": "US"}}})
	if status != http.StatusCreated {
		return status, saved, nil
	}
	paid, err := c.checkout(embedded, order{price: price, rail: "nmi", method: unwrap(saved)["id"].(string)})
	require.NoError(w.t, err)
	return status, saved, paid
}

// payWithCard is an order's pay naming payment, as a page that posts a card.
func (c *customer) payWithCard(price billing.PriceID, payment map[string]any) (int, map[string]any) {
	c.w.t.Helper()
	sold, err := c.sell(embedded, order{price: price})
	require.NoError(c.w.t, err)
	return c.call(http.MethodPost, "/orders/"+sold.order.ID.String()+"/pay", "card-"+uuid.NewString(), map[string]any{"payment": payment, "expected_total": micros(sold.order.Total)})
}

// cardVaults is the form of every Customer Vault request that carried a card
// number to the gateway.
func (w *world) cardVaults(action string) []url.Values {
	var out []url.Values
	for _, call := range w.nmi.CallsTo(http.MethodPost, "transact.php", func(v url.Values) bool { return v.Get("customer_vault") == action }) {
		out = append(out, call.Form)
	}
	return out
}

func (w *world) intentStatuses(intentType string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT status FROM billing.provider_intents WHERE intent_type = $1 ORDER BY created_at, id`), intentType)
	require.NoError(w.t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	return out
}

// refusalText is a refusal as the client reads it, less its request id: a
// random UUID that spells a card prefix such as "4111" by chance.
func refusalText(body map[string]any) string {
	out := maps.Clone(body)
	if e, ok := body["error"].(map[string]any); ok {
		e = maps.Clone(e)
		delete(e, "request_id")
		out["error"] = e
	}
	raw, _ := json.Marshal(out)
	return string(raw)
}

func errorOf(body map[string]any) (code, message string) {
	if e, ok := body["error"].(map[string]any); ok {
		code, _ = e["code"].(string)
		message, _ = e["message"].(string)
		return code, message
	}
	message, _ = body["error"].(string)
	return "", message
}

// A PSP advertises the card form its declaration names: the gateway's own
// fields by default, OpenRails' own (no gateway key) for card_entry: server.
func TestServerCardEntryIsAdvertised(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		world          func(*testing.T) *world
		flow           string
		tokenizerKeyed bool
	}{
		"browser": {func(t *testing.T) *world { return newWorld(t) }, "tokenize", true},
		"server":  {serverEntryWorld, "card", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := tc.world(t)
			config := publicConfig(t, w.rt)
			for _, psp := range config.Payment.PSPs {
				if psp.Rail != "nmi" {
					continue
				}
				require.Equal(t, tc.flow, psp.Flow)
				require.Equal(t, tc.tokenizerKeyed, psp.Config["tokenization_key"] != "")
				require.Equal(t, tc.tokenizerKeyed, psp.Config["tokenization_url"] != "", "a server PSP's page loads no gateway script")
			}
		})
	}
}

// With card_entry unset the firewall is unchanged: a card is refused on every
// route, whatever shape it arrives in, and nothing reaches the gateway.
func TestBrowserCardEntryRefusesCards(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	c := w.newCustomer()
	price := w.permanent("content:post")
	writes := len(w.nmi.Calls())
	const refusal = "card must be tokenized by the payment provider before calling OpenRails"

	status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "Card Holder"}})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	_, message := errorOf(body)
	require.Equal(t, refusal, message)

	// An order's payment takes no card.
	status, body = c.payWithCard(price.ID, map[string]any{"card": entryCard(entryVisa)})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.NotContains(t, refusalText(body), "4111")

	// A field a card number used to be pasted into is an unknown field, named
	// without its value.
	status, body = c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "token": w.nmi.Tokenize(visa), "card_number": entryVisa})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	code, message := errorOf(body)
	require.Equal(t, []string{"unknown_field", "unknown field card_number"}, []string{code, message})
	require.NotContains(t, refusalText(body), "4111")

	require.Len(t, w.nmi.Calls(), writes, "no refused card reached the gateway")
	require.Empty(t, w.cardVaults("add_customer"))
	require.Empty(t, w.intentStatuses("nmi_card_vault"))
	require.Empty(t, w.nmi.Unexpected())
}

// A server PSP takes the card and nothing else: never beside a token, never
// as a pasted number in another field, never through a Client.
func TestServerCardEntryAdmitsOnlyTheCardField(t *testing.T) {
	t.Parallel()
	w := serverEntryWorld(t)
	c := w.newCustomer()
	price := w.permanent("content:post")

	for name, body := range map[string]map[string]any{
		"card and token":           {"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "token": "tok-1234"},
		"number in another field":  {"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "4111 1111 1111 1111"}},
		"number as a bare field":   {"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "card_number": entryVisa},
		"number failing its check": {"psp_id": w.psp["nmi"], "card": map[string]any{"number": "4111111111111112", "exp_month": 10, "exp_year": 2027, "cvc": entryCVC}},
		"card without a code":      {"psp_id": w.psp["nmi"], "card": map[string]any{"number": entryVisa, "exp_month": 10, "exp_year": 2027}},
	} {
		status, out := c.call(http.MethodPost, "/payment-methods", "", body)
		require.Equal(t, http.StatusBadRequest, status, "%s: %v", name, out)
		require.NotContains(t, refusalText(out), "4111", "%s: a refusal never echoes the card", name)
	}
	status, out := c.payWithCard(price.ID, map[string]any{"card": entryCard(entryVisa), "token": "tok-1234"})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.NotContains(t, refusalText(out), "4111")

	// The merchant's API takes no card at all: a card is saved by its customer.
	status, out = w.hostJSON(http.MethodPost, "/v1/app/orders", map[string]any{"customer_id": c.id, "lines": []any{map[string]any{"price_id": price.ID}},
		"card": entryCard(entryVisa)})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	code, _ := errorOf(out)
	require.Equal(t, billing.CodeUnknownField, code)

	require.Empty(t, w.cardVaults("add_customer"), "no refused request reached the gateway")
	require.Empty(t, w.nmi.Vaults())
	require.Empty(t, w.nmi.Unexpected())
}

// A vault call whose answer is lost is settled by reading the vault the
// operation named; a call the gateway never saw leaves the card to be
// entered again. Neither sends the card twice.
func TestServerCardEntrySettlesALostAnswerByReadingTheGateway(t *testing.T) {
	t.Parallel()
	w := serverEntryWorld(t)
	c := w.newCustomer()
	save := map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "Card Holder"}}

	// The gateway vaulted the card; only its answer was lost.
	w.nmi.DropSaleResponses(1)
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", save))
	require.Len(t, w.cardVaults("add_customer"), 1, "the card was sent once")
	require.Equal(t, []string{"succeeded"}, w.intentStatuses("nmi_card_vault"))
	vault := w.vaultOf(saved["id"].(string))
	require.NotNil(t, w.nmi.Vault(vault), "the saved card is the vault the operation named")
	require.Equal(t, vault, w.cardVaults("add_customer")[0].Get("customer_vault_id"))

	// The gateway never saw the next request: its outcome is unknown now…
	w.nmi.LoseSales(1)
	status, body := c.call(http.MethodPost, "/payment-methods", "", save)
	require.Equal(t, http.StatusConflict, status, "%v", body)
	code, _ := errorOf(body)
	require.Equal(t, "provider_outcome_unknown", code)
	require.Equal(t, []string{"succeeded", "unknown_needs_verify"}, w.intentStatuses("nmi_card_vault"))
	require.Len(t, w.cardVaults("add_customer"), 1, "the lost request never arrived")

	// …and settled once the gateway has had its time: no vault, so the card
	// is entered again. The card itself is never sent a second time.
	w.advance(5 * time.Minute)
	w.wake()
	require.Equal(t, []string{"succeeded", "failed_terminal"}, w.intentStatuses("nmi_card_vault"))
	require.Len(t, w.cardVaults("add_customer"), 1)
	require.Len(t, w.nmi.Vaults(), 1)
	page, err := w.client[embedded].ListPaymentMethods(t.Context(), c.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	require.Len(t, page.Items, 1, "only the settled card is saved")
	require.Empty(t, w.nmi.Unexpected())
}

// A vault that surfaces after its request was told "unknown" is removed: the
// card does not stay at the gateway unreferenced.
func TestServerCardEntryRemovesAVaultThatSurfacesLate(t *testing.T) {
	t.Parallel()
	w := serverEntryWorld(t)
	c := w.newCustomer()

	// The gateway takes the request but acts on it only after the caller
	// has gone, and the reads in between do not show the vault.
	late := make(chan func(), 1)
	var once sync.Once
	w.nmi.Intercept(func(r *http.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "transact.php")
	},
		func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
			held := false
			once.Do(func() {
				held = true
				late <- func() { serve() }
			})
			if held {
				return nil, io.ErrUnexpectedEOF
			}
			return serve(), nil
		})
	status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "billing_details": map[string]any{"name": "Card Holder"}})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Empty(t, w.nmi.Vaults())

	(<-late)()
	require.Len(t, w.nmi.Vaults(), 1, "the gateway vaulted the card after all")
	w.advance(5 * time.Minute)
	w.wake()
	require.Equal(t, []string{"failed_terminal"}, w.intentStatuses("nmi_card_vault"))
	require.Empty(t, w.nmi.Vaults(), "the unreferenced vault is removed")
	page, err := w.client[embedded].ListPaymentMethods(t.Context(), c.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Empty(t, w.nmi.Unexpected())
}

// logTap collects every log line the process writes while a test holds it.
type logTap struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	active bool
}

var (
	tap     = &logTap{}
	tapOnce sync.Once
)

func (*logTap) Levels() []log.Level { return log.AllLevels }

func (l *logTap) Fire(e *log.Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.active {
		return nil
	}
	line, err := (&log.TextFormatter{DisableColors: true}).Format(e)
	if err != nil {
		return err
	}
	_, _ = l.buf.Write(line)
	// Structured sinks encode fields as JSON; capture that rendering too.
	if line, err = (&log.JSONFormatter{}).Format(e); err == nil {
		_, _ = l.buf.Write(line)
	}
	return nil
}

func (l *logTap) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active {
		_, _ = l.buf.Write(p)
	}
	return len(p), nil
}

// captureLogs records every logrus entry (at every level, in text and JSON)
// and every standard-library log line until the returned func is called.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	tapOnce.Do(func() { log.AddHook(tap) })
	level, std := log.GetLevel(), stdlog.Writer()
	tap.mu.Lock()
	tap.buf.Reset()
	tap.active = true
	tap.mu.Unlock()
	log.SetLevel(log.TraceLevel)
	stdlog.SetOutput(io.MultiWriter(std, tap))
	stop := func() string {
		log.SetLevel(level)
		stdlog.SetOutput(std)
		tap.mu.Lock()
		defer tap.mu.Unlock()
		tap.active = false
		return tap.buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

// cardTraces is every form a card number or its security code could take in
// stored or logged text: plain, grouped, hex and base64 for the number; the
// code as a standalone value.
func cardTraces(numbers ...string) *regexp.Regexp {
	var forms []string
	for _, n := range numbers {
		grouped := n[0:4] + "[ -]" + n[4:8] + "[ -]" + n[8:12] + "[ -]" + n[12:]
		forms = append(forms, n, grouped, hex.EncodeToString([]byte(n)), regexp.QuoteMeta(base64.StdEncoding.EncodeToString([]byte(n))))
	}
	forms = append(forms, `(^|[^0-9A-Za-z.-])`+entryCVC+`($|[^0-9A-Za-z-])`)
	return regexp.MustCompile(strings.Join(forms, "|"))
}

// tablesHolding names every table of the world's schema with a row matching
// pattern in any column, and how many.
func (w *world) tablesHolding(pattern string) (map[string]int, int) {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE' ORDER BY table_name`, w.schema)
	require.NoError(w.t, err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	hits := map[string]int{}
	for _, table := range tables {
		var n int
		name := pgx.Identifier{w.schema, table}.Sanitize()
		require.NoError(w.t, w.pool.QueryRow(w.t.Context(), `SELECT count(*) FROM `+name+` AS r WHERE r::text ~ $1`, pattern).Scan(&n), table)
		if n > 0 {
			hits[table] = n
		}
	}
	return hits, len(tables)
}

// THE TEST WALL (#1129). Cards go through every server-entry path — save,
// one-time purchase, subscription start and renewal, replacement, a decline,
// a refused request and a lost one — and then every table of the schema and
// every log line written meanwhile is searched for the numbers and the
// security code. Nothing is at rest: zero rows, zero lines.
//
// Not parallel: the log capture is the whole process's, so it runs alone.
func TestServerCardEntryLeavesNoCardAtRest(t *testing.T) {
	logs := captureLogs(t)
	w := serverEntryWorld(t)
	c := w.newCustomer()
	traces := cardTraces(entryVisa, entryMastercard)

	// Save a card: the customer route a payment-method panel posts to.
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa),
		"billing_details": map[string]any{"name": "Card Holder", "address": map[string]any{"line1": "1 Main St", "city": "Springfield", "state": "IL", "postal_code": "62701", "country": "US"}}}))
	method := saved["id"].(string)
	shown, _ := saved["card"].(map[string]any)
	require.Equal(t, []any{"visa", "1111", float64(10), float64(2027)}, []any{shown["brand"], shown["last4"], shown["exp_month"], shown["exp_year"]}, "%v", saved)
	vaults := w.cardVaults("add_customer")
	require.Len(t, vaults, 1)
	require.Equal(t, []string{entryVisa, "1027", entryCVC}, []string{vaults[0].Get("ccnumber"), vaults[0].Get("ccexp"), vaults[0].Get("cvv")}, "the gateway received the card")
	require.Equal(t, w.vaultOf(method), vaults[0].Get("customer_vault_id"))
	require.Equal(t, w.methodRow(method, "rail_method_ref"), vaults[0].Get("billing_id"))
	require.Empty(t, w.nmi.CallsTo(http.MethodPost, "/customers", nil), "no token vault call was made")
	require.Len(t, w.nmi.Validations(w.vaultOf(method)), 1, "the saved card is verified as a token-vaulted one is")

	// A one-time purchase with a new card, saved and then paid with.
	post := w.permanent("content:post")
	status, body, paid := w.relayPay(c, post.ID, entryCard(entryMastercard))
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.Equal(t, "succeeded", paid.Status, "%+v", paid.Order)
	w.settle()
	require.True(t, c.entitled("content:post"))
	require.Len(t, w.nmi.ledger(""), 1)
	require.Equal(t, "1111", w.nmi.LastSale().Card.Last4)
	require.Equal(t, "mastercard", w.nmi.LastSale().Card.Brand)

	// Another customer buys the same way.
	another := w.newCustomer()
	status, body, sold := w.relayPay(another, w.permanent("content:other").ID, entryCard(entryVisa))
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.Equal(t, "succeeded", sold.Status, "%+v", sold.Order)
	w.settle()
	require.Len(t, w.nmi.ledger(""), 2)

	// Subscription start with a new card, then a renewal on the vaulted card.
	member := w.newCustomer()
	price := w.membership("content:members", 9_990_000)
	status, body, enrolled := w.relayPay(member, price.ID, entryCard(entryVisa))
	require.Equal(t, http.StatusCreated, status, "%v", body)
	require.Equal(t, "succeeded", enrolled.Status, "%+v", enrolled.Order)
	w.settle()
	require.True(t, member.entitled("content:members"))
	sub := *enrolled.SubscriptionID
	end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
	w.advanceHealthyTo(end.Add(time.Minute))
	w.runRenewals()
	require.True(t, w.subscription(embedded, sub).CurrentPeriodEndsAt.After(end), "the renewal charged the vaulted card")
	require.Len(t, w.nmi.ledger(""), 4, "two purchases, the first period and its renewal")
	require.Len(t, w.cardVaults("add_customer"), 4, "each new card was vaulted once")

	// A card the issuer declines, one the gateway refuses to store, a request
	// the firewall refuses and one lost in transit.
	declined := w.newCustomer()
	w.nmi.Issue(entryVisa, card{Decline: "200"})
	status, body, _ = w.relayPay(declined, post.ID, entryCard(entryVisa))
	require.Equal(t, http.StatusPaymentRequired, status, "a definite decline: the buyer may try another card: %v", body)
	w.nmi.Issue(entryVisa, card{Decline: "vault"})
	status, body = declined.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa)})
	require.Equal(t, http.StatusBadGateway, status, "a gateway rejection, as a rejected sale is: %v", body)
	w.nmi.Issue(entryVisa, card{})
	status, body = declined.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa), "token": "tok-1234"})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	w.nmi.LoseSales(1)
	status, body = declined.call(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"], "card": entryCard(entryVisa)})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	w.advance(5 * time.Minute)
	w.wake()
	page, err := w.client[embedded].ListPaymentMethods(t.Context(), declined.cid(), billing.PaymentMethodListParams{PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(t, err)
	require.Empty(t, page.Items, "no refused, declined or lost card is saved")
	require.Empty(t, w.nmi.Unexpected())

	// The wall. The scan is proven able to find a card first: the same search
	// over a row that holds one must hit.
	_, err = w.pool.Exec(t.Context(), w.q(`CREATE TABLE billing.card_wall_probe (note text)`))
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.card_wall_probe VALUES ($1), ($2), ($3)`),
		`{"ccnumber":"`+entryMastercard+`"}`, "cvv="+entryCVC, "paid with 4111-1111-1111-1111")
	require.NoError(t, err)
	hits, tables := w.tablesHolding(traces.String())
	require.Equal(t, map[string]int{"card_wall_probe": 3}, hits, "the only rows holding a card are the probe's own")
	require.Greater(t, tables, 60, "every table of the schema was searched")
	_, err = w.pool.Exec(t.Context(), w.q(`DROP TABLE billing.card_wall_probe`))
	require.NoError(t, err)

	written := logs()
	require.Contains(t, written, "Payment method create timing", "the capture saw the requests' own log lines")
	require.True(t, traces.MatchString("cvv="+entryCVC) && traces.MatchString(hex.EncodeToString([]byte(entryVisa))), "the search finds a logged card")
	if found := traces.FindAllString(written, -1); len(found) > 0 {
		t.Fatalf("%d log lines carry card data", len(found))
	}
}
