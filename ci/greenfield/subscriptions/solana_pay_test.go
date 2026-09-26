//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/solanafake"
)

// #1086: Solana Pay state lives in PostgreSQL. Every replica sees a checkout's
// one reference, a signature credits at most one checkout once, and money that
// does not settle a checkout is recorded for review instead of being lost.

type solanaPay struct {
	w     *world
	fake  *solanafake.Node
	price string
	rail  openrails.CheckoutRailOption
	// stopWorkers ends the poller loop of each running runtime, by world.
	stopWorkers map[*world]func()
}

// transferRequest is what a buyer's wallet reads from the Solana Pay URL.
type transferRequest struct {
	buyer     *customer
	session   *openrails.CheckoutSession
	recipient string
	mint      string
	amount    uint64
	reference string
	memo      string
}

func newSolanaPay(t *testing.T) *solanaPay {
	w := prepareWorld(t, 30)
	fake, _ := withSolana(t, w)
	w.mount = func(c *embed.HTTPConfig) { c.Checkout = true }
	w.start()
	p := &solanaPay{w: w, fake: fake, stopWorkers: map[*world]func(){}}
	p.runWorkers(w)
	key, err := w.applyCatalog(`  - key: "{key}-once"
    currency: usd
    unit_amount: 5000000
    auto_renew: false
    access_duration_hours: 720
    psps: [solana]
`)
	require.NoError(t, err)
	p.price = priceID(t, w, key+"-once")
	p.rail = w.options(key + "-once")["solana"]
	require.NotEmpty(t, p.rail.PSPID, "solana is offered")
	return p
}

// runWorkers runs a runtime's non-River loops (the Solana Pay poller), as a
// host does with Runtime.RunWorkers.
func (p *solanaPay) runWorkers(w *world) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(w.t.Context()))
	done := make(chan struct{})
	rt := w.rt
	go func() { defer close(done); _ = rt.RunWorkers(ctx) }()
	stop := func() { cancel(); <-done }
	p.stopWorkers[w] = stop
	w.t.Cleanup(stop)
}

// replica starts a second process over the same database and chain.
func (p *solanaPay) replica() *world {
	b := p.w
	r := &world{t: b.t, pool: b.pool, dsn: b.dsn, schema: b.schema, slug: b.slug, stripe: b.stripe, nmi: b.nmi, auth: b.auth,
		cfg: b.cfg, declare: b.declare, mount: b.mount, clock: b.clock, queries: b.queries}
	r.start()
	b.t.Cleanup(r.stop)
	p.runWorkers(r)
	return r
}

// restart stops a process mid-flight and starts it again.
func (p *solanaPay) restart(w *world) {
	p.stopWorkers[w]()
	w.stop()
	w.start()
	p.runWorkers(w)
}

func (p *solanaPay) checkout(buyer *customer) transferRequest {
	t := p.w.t
	session, err := p.w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: buyer.id}, PriceID: p.price, IdempotencyKey: "sol-" + uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: p.rail.Selector, PSPID: p.rail.PSPID, TokenSymbol: "DUSD", Flow: "transfer_request"},
		SuccessURL:     "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(t, err)
	require.Equal(t, "requires_action", session.Status)
	raw, _ := session.RailData["transaction_url"].(string)
	u, err := url.Parse(strings.Replace(raw, "solana:", "solana://", 1))
	require.NoError(t, err, raw)
	q := u.Query()
	amount, ok := new(big.Rat).SetString(q.Get("amount"))
	require.True(t, ok, raw)
	units := new(big.Rat).Mul(amount, big.NewRat(1_000_000, 1))
	require.True(t, units.IsInt(), raw)
	return transferRequest{buyer: buyer, session: session, recipient: u.Host, mint: q.Get("spl-token"), amount: units.Num().Uint64(), reference: q.Get("reference"), memo: q.Get("memo")}
}

// pay lands the wallet's transfer for amount at the engine's current time.
func (p *solanaPay) pay(req transferRequest, amount uint64) string {
	sig, err := p.fake.Land(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint,
		Amount: amount, Reference: req.reference, Memo: req.memo, BlockTime: p.w.clock.Now()})
	require.NoError(p.w.t, err)
	return sig
}

func (p *solanaPay) sql(query string) string {
	return strings.ReplaceAll(query, "$schema", pgx.Identifier{p.w.schema}.Sanitize())
}

func (p *solanaPay) status(req transferRequest) string {
	var status string
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT status FROM $schema.checkout_sessions WHERE id = $1`), uuidOf(p.w.t, req.session.ID)).Scan(&status))
	return status
}

func (p *solanaPay) referenceStatus(req transferRequest) string {
	var status string
	err := p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT status FROM $schema.solana_pay_references WHERE reference = $1`), req.reference).Scan(&status)
	if err == pgx.ErrNoRows {
		return "gone"
	}
	require.NoError(p.w.t, err)
	return status
}

func (p *solanaPay) receipt(sig string) (disposition, reason string) {
	var r *string
	err := p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT disposition, review_reason FROM $schema.solana_pay_receipts WHERE signature = $1`), sig).Scan(&disposition, &r)
	if err == pgx.ErrNoRows {
		return "", ""
	}
	require.NoError(p.w.t, err)
	if r != nil {
		reason = *r
	}
	return disposition, reason
}

func (p *solanaPay) payments(req transferRequest) int {
	var n int
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT count(*) FROM $schema.payments WHERE rail = 'solana' AND metadata->>'checkout_session_id' = $1 AND deleted_at IS NULL`),
		uuidOf(p.w.t, req.session.ID).String()).Scan(&n))
	return n
}

func (p *solanaPay) reviewAlerts(sig string) int {
	var n int
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT count(*) FROM $schema.notifications WHERE data->>'transaction_id' = $1`), sig).Scan(&n))
	return n
}

func (p *solanaPay) eventually(cond func() bool, what string) {
	p.w.t.Helper()
	require.Eventually(p.w.t, cond, 20*time.Second, 50*time.Millisecond, what)
}

func uuidOf(t *testing.T, id string) uuid.UUID {
	parsed, err := openrails.ParseCheckoutSessionID(id)
	require.NoError(t, err)
	return parsed.UUID()
}

// Two replicas' pollers and a burst of client confirmations race on one
// landed signature: the checkout is credited once.
func TestSolanaPayConcurrentConfirmationsCreditOnce(t *testing.T) {
	p := newSolanaPay(t)
	second := p.replica()
	buyer := p.w.newCustomer()
	req := p.checkout(buyer)
	sig := p.pay(req, req.amount)

	var wg sync.WaitGroup
	for i := range 8 {
		client := p.w.client[embedded]
		if i%2 == 1 {
			client = second.client[embedded]
		}
		wg.Go(func() {
			session, err := client.ConfirmCheckoutSession(t.Context(), req.session.ID, openrails.ConfirmCheckoutSessionRequest{
				CustomerID: buyer.id, Payment: openrails.ConfirmPayment{Rail: "solana", Signature: sig},
			})
			if assert.NoError(t, err) {
				assert.Equal(t, "succeeded", session.Status)
			}
		})
	}
	wg.Wait()

	p.eventually(func() bool { return p.status(req) == "succeeded" }, "checkout credited")
	disposition, reason := p.receipt(sig)
	require.Equal(t, "credited", disposition)
	require.Empty(t, reason)
	require.Equal(t, 1, p.payments(req), "one payment for one signature")
	require.Equal(t, "confirmed", p.referenceStatus(req))
}

// A second transfer to a paid reference is recorded for refund review and
// never credited; the first stays the one payment.
func TestSolanaPaySecondTransferIsFlagged(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	first := p.pay(req, req.amount)
	p.eventually(func() bool { return p.status(req) == "succeeded" }, "first transfer credited")

	second := p.pay(req, req.amount)
	p.eventually(func() bool {
		p.w.clock.Advance(2 * time.Minute)
		d, _ := p.receipt(second)
		return d != ""
	}, "second transfer recorded")
	disposition, reason := p.receipt(second)
	require.Equal(t, "review", disposition)
	require.Equal(t, "already_paid", reason)
	d, _ := p.receipt(first)
	require.Equal(t, "credited", d)
	require.Equal(t, 1, p.payments(req))
	require.Equal(t, 1, p.reviewAlerts(second), "operators are told about the money to refund")

	// Submitting the second signature from the page answers with the review,
	// not a second credit.
	_, err := p.w.client[embedded].ConfirmCheckoutSession(t.Context(), req.session.ID, openrails.ConfirmCheckoutSessionRequest{
		CustomerID: req.buyer.id, Payment: openrails.ConfirmPayment{Rail: "solana", Signature: second},
	})
	require.Error(t, err)
	require.Equal(t, 1, p.payments(req))
}

// A transfer that lands after the quote and its grace expired is recorded
// for review; the checkout is not credited.
func TestSolanaPayLatePaymentIsFlagged(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	p.w.clock.Advance(15*time.Minute + 30*time.Minute + time.Minute)
	p.eventually(func() bool { return p.referenceStatus(req) == "expired" }, "reference expired")

	late := p.pay(req, req.amount)
	p.eventually(func() bool {
		p.w.clock.Advance(15 * time.Minute)
		d, _ := p.receipt(late)
		return d != ""
	}, "late transfer recorded")
	disposition, reason := p.receipt(late)
	require.Equal(t, "review", disposition)
	require.Equal(t, "late", reason)
	require.Equal(t, 0, p.payments(req))
	require.NotEqual(t, "succeeded", p.status(req))
	require.Equal(t, 1, p.reviewAlerts(late))
}

// Short and excess transfers are recorded: underpaid is not credited,
// overpaid is credited with the excess flagged.
func TestSolanaPayAmountMismatch(t *testing.T) {
	p := newSolanaPay(t)
	short := p.checkout(p.w.newCustomer())
	under := p.pay(short, short.amount-1)
	p.eventually(func() bool { d, _ := p.receipt(under); return d != "" }, "underpayment recorded")
	d, reason := p.receipt(under)
	require.Equal(t, []string{"review", "underpaid"}, []string{d, reason})
	require.Equal(t, 0, p.payments(short))
	require.Equal(t, "pending", p.referenceStatus(short), "the checkout can still be paid in full")

	extra := p.checkout(p.w.newCustomer())
	over := p.pay(extra, extra.amount+1)
	p.eventually(func() bool { return p.status(extra) == "succeeded" }, "overpayment credited")
	d, reason = p.receipt(over)
	require.Equal(t, []string{"credited", "overpaid"}, []string{d, reason})
	require.Equal(t, 1, p.reviewAlerts(over))
}

// The only replica stops while a checkout awaits payment and the wallet pays
// meanwhile; after the restart the pending state is intact and the transfer
// is credited.
func TestSolanaPayPendingSurvivesRestart(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	require.Equal(t, "pending", p.referenceStatus(req))

	p.stopWorkers[p.w]()
	p.w.stop()
	sig := p.pay(req, req.amount)
	p.w.start()
	p.runWorkers(p.w)

	p.eventually(func() bool { return p.status(req) == "succeeded" }, "credited after restart")
	d, _ := p.receipt(sig)
	require.Equal(t, "credited", d)
	require.Equal(t, 1, p.payments(req))
}

type solanaPayGC struct{}

func (solanaPayGC) Kind() string { return "openrails.solana_pay_gc" }

// The GC job deletes settled references past their watch window and the
// ignored receipts that went with them; pending references and credited or
// review receipts stay.
func TestSolanaPayGCRemovesOnlySettledRows(t *testing.T) {
	p := newSolanaPay(t)
	paid := p.checkout(p.w.newCustomer())
	credit := p.pay(paid, paid.amount)
	p.eventually(func() bool { return p.status(paid) == "succeeded" }, "paid")
	stray, err := p.fake.Land(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: solanago.NewWallet().PublicKey().String(),
		Mint: paid.mint, Amount: 1, Reference: paid.reference, Memo: paid.memo, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	p.eventually(func() bool {
		p.w.clock.Advance(2 * time.Minute)
		d, _ := p.receipt(stray)
		return d == "ignored"
	}, "a transfer to someone else is ignored")
	abandoned := p.checkout(p.w.newCustomer())
	p.w.clock.Advance(time.Hour)
	p.eventually(func() bool { return p.referenceStatus(abandoned) == "expired" }, "abandoned checkout expired")

	p.w.clock.Advance(8 * 24 * time.Hour)
	fresh := p.checkout(p.w.newCustomer())

	res, err := p.w.jobs.Insert(t.Context(), solanaPayGC{}, &river.InsertOpts{Queue: embed.QueueBilling})
	require.NoError(t, err)
	p.w.waitJob(res.Job.ID)

	require.Equal(t, "gone", p.referenceStatus(paid))
	require.Equal(t, "gone", p.referenceStatus(abandoned))
	require.Equal(t, "pending", p.referenceStatus(fresh), "a pending reference is never collected")
	d, _ := p.receipt(credit)
	require.Equal(t, "credited", d, "the credited receipt is kept")
	d, _ = p.receipt(stray)
	require.Empty(t, d, "the ignored receipt went with its reference")
	require.Equal(t, 1, p.payments(paid))
}

// A transaction-request checkout offers one payable transaction at a time:
// the same one while its blockhash can land, a new one only after the chain
// can no longer include it and nothing landed, and none once a transfer is on
// the reference.
func TestSolanaPayOffersOneTransactionPerAttempt(t *testing.T) {
	p := newSolanaPay(t)
	buyer := p.w.newCustomer()
	session, err := p.w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		Customer: openrails.CheckoutCustomerIdentity{ID: buyer.id}, PriceID: p.price, IdempotencyKey: "sol-" + uuid.NewString(),
		PaymentOptions: openrails.CheckoutPaymentOptions{Rail: p.rail.Selector, PSPID: p.rail.PSPID, TokenSymbol: "DUSD", Flow: "transaction_request"},
		SuccessURL:     "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(t, err)
	wallet := solanago.NewWallet().PublicKey()
	post := func() (int, string) {
		raw, err := json.Marshal(map[string]string{"account": wallet.String()})
		require.NoError(t, err)
		res, err := http.Post(p.w.server.URL+mountPrefix+"/v1/checkout/"+session.ID+"/solana-pay", "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		defer res.Body.Close()
		var body struct {
			Transaction string `json:"transaction"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res.StatusCode, body.Transaction
	}

	status, first := post()
	require.Equal(t, http.StatusOK, status)
	status, again := post()
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, first, again, "a second request gets the same transaction while it can land")

	p.fake.AdvanceBlocks(200)
	status, rebuilt := post()
	require.Equal(t, http.StatusOK, status)
	require.NotEqual(t, first, rebuilt, "an unlandable transaction is replaced")

	p.stopWorkers[p.w]()
	var recipient, mint, reference, amount string
	require.NoError(t, p.w.pool.QueryRow(t.Context(), p.sql(`SELECT rail_state->>'recipient', rail_state->>'token_mint', reference, rail_state->>'token_amount' FROM $schema.checkout_sessions WHERE id = $1`),
		uuidOf(t, session.ID)).Scan(&recipient, &mint, &reference, &amount))
	units, err := strconv.ParseUint(amount, 10, 64)
	require.NoError(t, err)
	req := transferRequest{buyer: buyer, session: session, recipient: recipient, mint: mint, amount: units, reference: reference, memo: solanaint.PurchaseMemo(uuidOf(t, session.ID))}
	sig := p.pay(req, units)
	p.fake.AdvanceBlocks(200)
	status, _ = post()
	require.Equal(t, http.StatusConflict, status, "no new transaction once a transfer landed")

	p.runWorkers(p.w)
	p.eventually(func() bool {
		p.w.clock.Advance(time.Second)
		return p.status(req) == "succeeded"
	}, "the landed transfer is credited")
	d, _ := p.receipt(sig)
	require.Equal(t, "credited", d)
	require.Equal(t, 1, p.payments(req))
	status, _ = post()
	require.Equal(t, http.StatusConflict, status)
}

// One transfer naming two checkouts' references settles at most one of them:
// the signature is credited once, and the other checkout stays unpaid.
func TestSolanaPayOneTransferCreditsOneCheckout(t *testing.T) {
	p := newSolanaPay(t)
	buyer := p.w.newCustomer()
	a, b := p.checkout(buyer), p.checkout(buyer)
	require.Equal(t, a.amount, b.amount)
	sig, err := p.fake.Land(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: a.recipient, Mint: a.mint,
		Amount: a.amount, Reference: a.reference, Also: []string{b.reference}, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)

	p.eventually(func() bool { return p.status(a) == "succeeded" || p.status(b) == "succeeded" }, "one checkout credited")
	p.eventually(func() bool {
		p.w.clock.Advance(2 * time.Minute)
		return p.referenceStatus(a) != "pending" || p.referenceStatus(b) != "pending"
	}, "both references read")
	for range 3 {
		p.w.clock.Advance(4 * time.Second)
		time.Sleep(time.Second)
	}
	require.Equal(t, 1, p.payments(a)+p.payments(b), "one transfer, one payment")
	var receipts int
	require.NoError(t, p.w.pool.QueryRow(t.Context(), p.sql(`SELECT count(*) FROM $schema.solana_pay_receipts WHERE signature = $1`), sig).Scan(&receipts))
	require.Equal(t, 1, receipts, "the signature settles one reference")
	require.NotEqual(t, p.status(a), p.status(b), "the other checkout is still unpaid")
}
