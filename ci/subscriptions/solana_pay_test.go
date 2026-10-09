//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hosttools"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/solanafake"
)

// #1086: Solana Pay state lives in PostgreSQL. Every replica sees a checkout's
// one reference, a signature credits at most one checkout once, and money that
// does not settle a checkout is recorded for review instead of being lost.

type solanaPay struct {
	w     *world
	fake  *solanafake.Node
	price string
	// prices sell the one-time pass in each token: DUSD, the merchant's
	// first stablecoin, for its USD price, and PYUSD for a PYUSD price.
	prices map[string]string
	rail   billing.CheckoutOption
	// stopWorkers ends the poller loop of each running runtime, by world.
	stopWorkers map[*world]func()
}

// transferRequest is what a buyer's wallet reads from the Solana Pay URL.
type transferRequest struct {
	buyer *customer
	// id is the checkout attempt the session's payment created.
	id        billing.CheckoutAttemptID
	recipient string
	mint      string
	amount    uint64
	reference string
	memo      string
}

func newSolanaPay(t *testing.T) *solanaPay {
	w := prepareWorld(t, 30)
	fake, _ := withSolana(t, w)
	declared := w.declare
	w.declare = func(psps map[string]openrails.PSPConfig) {
		declared(psps)
		psps["solana"].Settings["tokens"].(map[string]any)["PYUSD"] = map[string]any{} // Token-2022
	}
	w.start()
	p := &solanaPay{w: w, fake: fake, stopWorkers: map[*world]func(){}}
	p.runWorkers(w)
	key, err := w.applyCatalog(`  "{key}-once":
    currency: usd
    unit_amount: 5000000
    billing_interval_hours: null
    access_duration_hours: 720
    psps: [solana]
  "{key}-pyusd":
    amount: 5 PYUSD
    billing_interval_hours: null
    access_duration_hours: 720
    psps: [solana]
`)
	require.NoError(t, err)
	p.price = priceID(t, w, key, key+"-once")
	p.prices = map[string]string{"DUSD": p.price, "PYUSD": priceID(t, w, key, key+"-pyusd")}
	p.rail = w.options(billing.CheckoutOptionListParams{ProductKey: key, PriceKey: key + "-once"})["solana"]
	require.NotEmpty(t, p.rail.PSPID, "solana is offered")
	return p
}

// runWorkers runs a runtime's non-River loops (the Solana Pay poller), as a
// Client.Start runs them for a host; the test stops them on its own.
func (p *solanaPay) runWorkers(w *world) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(w.t.Context()))
	done := make(chan struct{})
	rt := w.rt
	go func() { defer close(done); _ = engine.Graph(rt).Runtime.RunWorkers(ctx) }()
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
	return p.checkoutIn(buyer, "DUSD")
}

// checkoutIn is buyer paying a session for the pass in token: the page shows
// the Solana Pay transfer request its wallet pays.
func (p *solanaPay) checkoutIn(buyer *customer, token string) transferRequest {
	t := p.w.t
	price, ok := p.prices[token]
	require.True(t, ok, "no price in %s", token)
	paid, err := buyer.checkout(embedded, order{price: pid(price), rail: "solana", successURL: "https://e2e.test/return"})
	require.NoError(t, err)
	require.Equal(t, "requires_action", paid.Status)
	require.NotNil(t, paid.NextAction, "%+v", paid.CheckoutSessionPayResult)
	require.Equal(t, "solana_pay", paid.NextAction.Type)
	raw := *paid.NextAction.URL
	u, err := url.Parse(strings.Replace(raw, "solana:", "solana://", 1))
	require.NoError(t, err, raw)
	q := u.Query()
	amount, ok := new(big.Rat).SetString(q.Get("amount"))
	require.True(t, ok, raw)
	units := new(big.Rat).Mul(amount, big.NewRat(1_000_000, 1))
	require.True(t, units.IsInt(), raw)
	return transferRequest{buyer: buyer, id: paid.session.attemptID(), recipient: u.Host, mint: q.Get("spl-token"), amount: units.Num().Uint64(), reference: q.Get("reference"), memo: q.Get("memo")}
}

// pay lands the wallet's transfer for amount at the engine's current time.
func (p *solanaPay) pay(req transferRequest, amount uint64) string {
	sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint,
		Amount: amount, Reference: req.reference, Memo: req.memo, BlockTime: p.w.clock.Now()})
	require.NoError(p.w.t, err)
	return sig
}

func (p *solanaPay) sql(query string) string {
	return strings.ReplaceAll(query, "$schema", pgx.Identifier{p.w.schema}.Sanitize())
}

func (p *solanaPay) status(req transferRequest) string {
	var status string
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT status FROM $schema.checkout_attempts WHERE id = $1`), uuidOf(p.w.t, req.id)).Scan(&status))
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

// receiptOn is the disposition recorded for sig on req's reference.
func (p *solanaPay) receiptOn(req transferRequest, sig string) string {
	var d string
	err := p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT disposition FROM $schema.solana_pay_receipts WHERE reference = $1 AND signature = $2`), req.reference, sig).Scan(&d)
	if err == pgx.ErrNoRows {
		return ""
	}
	require.NoError(p.w.t, err)
	return d
}

func (p *solanaPay) payments(req transferRequest) int {
	var n int
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT count(*) FROM $schema.payments WHERE rail = 'solana' AND metadata->>'checkout_attempt_id' = $1 AND deleted_at IS NULL`),
		uuidOf(p.w.t, req.id).String()).Scan(&n))
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

func uuidOf(_ *testing.T, id billing.CheckoutAttemptID) uuid.UUID { return id.UUID() }

// Two replicas' pollers and a burst of relayed confirmations race on one
// landed signature: the checkout is credited once.
func TestSolanaPayConcurrentConfirmationsCreditOnce(t *testing.T) {
	p := newSolanaPay(t)
	second := p.replica()
	buyer := p.w.newCustomer()
	req := p.checkout(buyer)
	sig := p.pay(req, req.amount)

	var wg sync.WaitGroup
	for i := range 8 {
		w := p.w
		if i%2 == 1 {
			w = second
		}
		wg.Go(func() {
			confirmed, err := w.engineConfirm(req.id, sig)
			if assert.NoError(t, err) {
				assert.Equal(t, "succeeded", confirmed.Status)
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

	// Relaying the second signature answers with the review, not a second
	// credit.
	_, err := p.w.engineConfirm(req.id, second)
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
	stray, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: solanago.NewWallet().PublicKey().String(),
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

	res, err := p.w.jobs.Insert(t.Context(), solanaPayGC{}, &river.InsertOpts{Queue: openrails.QueueBilling})
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
	session, err := p.w.engineCheckout(buyer, p.transactionRequestFor("DUSD"))
	require.NoError(t, err)
	wallet := solanago.NewWallet().PublicKey()
	post := func() (int, string) {
		raw, err := json.Marshal(map[string]string{"account": wallet.String()})
		require.NoError(t, err)
		res, err := http.Post(p.w.server.URL+mountPrefix+"/v1/checkout-attempts/"+session.ID.String()+"/solana-pay", "application/json", bytes.NewReader(raw))
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
	require.NoError(t, p.w.pool.QueryRow(t.Context(), p.sql(`SELECT rail_state->>'recipient', rail_state->>'token_mint', reference, rail_state->>'token_amount' FROM $schema.checkout_attempts WHERE id = $1`),
		uuidOf(t, session.ID)).Scan(&recipient, &mint, &reference, &amount))
	units, err := strconv.ParseUint(amount, 10, 64)
	require.NoError(t, err)
	req := transferRequest{buyer: buyer, id: session.ID, recipient: recipient, mint: mint, amount: units, reference: reference, memo: solanaint.PurchaseMemo(uuidOf(t, session.ID))}
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
	sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: a.recipient, Mint: a.mint,
		Amount: a.amount, Reference: a.reference, Also: []string{b.reference}, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)

	p.eventually(func() bool { return p.status(a) == "succeeded" || p.status(b) == "succeeded" }, "one checkout credited")
	p.eventually(func() bool {
		p.w.clock.Advance(2 * time.Minute)
		return p.referenceStatus(a) != "pending" || p.referenceStatus(b) != "pending"
	}, "both references read")
	p.eventually(func() bool {
		p.w.clock.Advance(4 * time.Second)
		return p.receiptOn(a, sig) != "" && p.receiptOn(b, sig) != ""
	}, "the transfer is recorded on both references")
	require.Equal(t, 1, p.payments(a)+p.payments(b), "one transfer, one payment")
	require.ElementsMatch(t, []string{"credited", "duplicate"}, []string{p.receiptOn(a, sig), p.receiptOn(b, sig)},
		"the other reference records that the transfer settled a different checkout")
	require.NotEqual(t, p.status(a), p.status(b), "the other checkout is still unpaid")
}

// transactionRequestFor is a one-off Solana transaction request for the pass
// in token. No checkout session offers one (a session's Solana option is a
// transfer request), so it is created straight on the engine.
func (p *solanaPay) transactionRequestFor(token string) checkout.CheckoutAttemptCreateRequest {
	return checkout.CheckoutAttemptCreateRequest{PriceID: p.price, SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
		Payment: checkout.CheckoutAttemptPaymentRequest{Rail: p.rail.PSP, TokenSymbol: token, Flow: "transaction_request"}}
}

// transactionRequest opens a transaction-request checkout and returns the
// wallet's POST to its Solana Pay endpoint.
func (p *solanaPay) transactionRequest(buyer *customer, token string, wallet solanago.PublicKey) (billing.CheckoutAttemptID, func() (int, string)) {
	t := p.w.t
	session, err := p.w.engineCheckout(buyer, p.transactionRequestFor(token))
	require.NoError(t, err)
	return session.ID, func() (int, string) {
		raw, err := json.Marshal(map[string]string{"account": wallet.String()})
		require.NoError(t, err)
		res, err := http.Post(p.w.server.URL+mountPrefix+"/v1/checkout-attempts/"+session.ID.String()+"/solana-pay", "application/json", bytes.NewReader(raw))
		require.NoError(t, err)
		defer res.Body.Close()
		var body struct {
			Transaction string `json:"transaction"`
		}
		_ = json.NewDecoder(res.Body).Decode(&body)
		return res.StatusCode, body.Transaction
	}
}

// sessionTerms reads the quote a checkout attempt was bound to.
func (p *solanaPay) sessionTerms(buyer *customer, id billing.CheckoutAttemptID) transferRequest {
	var recipient, mint, reference, amount string
	require.NoError(p.w.t, p.w.pool.QueryRow(p.w.t.Context(), p.sql(`SELECT rail_state->>'recipient', rail_state->>'token_mint', reference, rail_state->>'token_amount' FROM $schema.checkout_attempts WHERE id = $1`),
		uuidOf(p.w.t, id)).Scan(&recipient, &mint, &reference, &amount))
	units, err := strconv.ParseUint(amount, 10, 64)
	require.NoError(p.w.t, err)
	return transferRequest{buyer: buyer, id: id, recipient: recipient, mint: mint, amount: units, reference: reference, memo: solanaint.PurchaseMemo(uuidOf(p.w.t, id))}
}

func (p *solanaPay) until(cond func() bool, what string) {
	p.w.t.Helper()
	p.eventually(func() bool {
		p.w.clock.Advance(4 * time.Second)
		return cond()
	}, what)
}

// An attacker's own payment that also names a victim's reference cannot
// block the victim: the stolen reference records the transfer as settling
// another checkout and the victim's real payment is still credited.
func TestSolanaPayPoisonedReferenceStillCredits(t *testing.T) {
	p := newSolanaPay(t)
	victim := p.checkout(p.w.newCustomer())
	attacker := p.checkout(p.w.newCustomer())
	// The attacker pays their own checkout, confirming it with its signature
	// before any poller reads the victim's reference.
	p.stopWorkers[p.w]()
	attack, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: attacker.recipient, Mint: attacker.mint,
		Amount: attacker.amount, Reference: attacker.reference, Also: []string{victim.reference}, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	_, err = p.w.engineConfirm(attacker.id, attack)
	require.NoError(t, err)
	p.runWorkers(p.w)
	p.until(func() bool { return p.receiptOn(victim, attack) != "" }, "the attack is read on the victim's reference")
	require.Equal(t, "credited", p.receiptOn(attacker, attack), "the attacker paid for their own checkout")
	require.Equal(t, "duplicate", p.receiptOn(victim, attack), "the victim's reference records the transfer as settling another checkout")

	paid := p.pay(victim, victim.amount)
	p.until(func() bool { return p.status(victim) == "succeeded" }, "the victim's own payment is credited")
	require.Equal(t, "credited", p.receiptOn(victim, paid))
	require.Equal(t, 1, p.payments(victim))
}

// A versioned (v0) transaction is read, never left to poison the reference.
func TestSolanaPayVersionedTransactionCredited(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint,
		Amount: req.amount, Reference: req.reference, Memo: req.memo, V0: true, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	p.until(func() bool { return p.status(req) == "succeeded" }, "the v0 payment is credited")
	require.Equal(t, "credited", p.receiptOn(req, sig))
}

// Token-2022 mints (PYUSD, USDG) pay into accounts their own program derives:
// a wallet's transfer is credited, and the transaction OpenRails builds calls
// Token-2022 into the recipient's Token-2022 account.
func TestSolanaPayToken2022(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkoutIn(p.w.newCustomer(), "PYUSD")
	require.Equal(t, solanafake.DevnetPYUSDMint, req.mint)
	sig := p.pay(req, req.amount)
	p.until(func() bool { return p.status(req) == "succeeded" }, "the Token-2022 payment is credited")
	require.Equal(t, "credited", p.receiptOn(req, sig))

	wallet := solanago.NewWallet().PublicKey()
	session, post := p.transactionRequest(p.w.newCustomer(), "PYUSD", wallet)
	status, encoded := post()
	require.Equal(t, http.StatusOK, status)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(raw)
	require.NoError(t, err)
	terms := p.sessionTerms(nil, session)
	recipient := solanago.MustPublicKeyFromBase58(terms.recipient)
	mint := solanago.MustPublicKeyFromBase58(solanafake.DevnetPYUSDMint)
	dest, err := solanaint.AssociatedTokenAddress(recipient, mint, solanaint.Token2022ProgramID)
	require.NoError(t, err)
	transfer := tx.Message.Instructions[len(tx.Message.Instructions)-1]
	program, err := tx.ResolveProgramIDIndex(transfer.ProgramIDIndex)
	require.NoError(t, err)
	require.Equal(t, solanaint.Token2022ProgramID, program)
	accounts, err := transfer.ResolveInstructionAccounts(&tx.Message)
	require.NoError(t, err)
	require.Equal(t, dest, accounts[2].PublicKey, "paid into the recipient's Token-2022 account")
}

// spam lands n failed transactions naming the reference.
func (p *solanaPay) spam(req transferRequest, n int) []string {
	sigs := make([]string, 0, n)
	for range n {
		sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint,
			Amount: 1, Reference: req.reference, Failed: true, BlockTime: p.w.clock.Now()})
		require.NoError(p.w.t, err)
		sigs = append(sigs, sig)
	}
	return sigs
}

// However many transactions name the reference, before or after the payment
// and on either side of a page boundary, the walk reads the whole history.
func TestSolanaPayWalkReadsTheWholeHistory(t *testing.T) {
	p := newSolanaPay(t)
	for _, c := range []struct {
		name          string
		before, after int
	}{
		{"payment newest, 1500 before", 1_500, 0},
		{"payment oldest, a page exactly", 0, 999},
		{"a page before and after", 1_000, 1_000},
		{"many pages after", 0, 6_500},
	} {
		req := p.checkout(p.w.newCustomer())
		p.stopWorkers[p.w]() // the whole history lands before any read
		p.spam(req, c.before)
		sig := p.pay(req, req.amount)
		p.spam(req, c.after)
		p.runWorkers(p.w)
		p.until(func() bool { return p.status(req) == "succeeded" }, c.name)
		require.Equal(t, "credited", p.receiptOn(req, sig), c.name)
	}
}

// A node that has lost the walk's cursor answers the page below it with
// nothing; that is not proof the history ended, and the payment under it is
// still found once a node that holds it answers.
func TestSolanaPayDroppedReadDoesNotSkipHistory(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	p.stopWorkers[p.w]()
	sig := p.pay(req, req.amount)
	spam := p.spam(req, 1_000)
	p.fake.Forget(spam[0], 2) // the cursor below the newest page
	p.runWorkers(p.w)
	p.until(func() bool { return p.status(req) == "succeeded" }, "the payment below the dropped read is credited")
	require.Equal(t, "credited", p.receiptOn(req, sig))
}

// A settled reference whose history walk has not finished is never collected.
func TestSolanaPayGCKeepsAnUnfinishedWalk(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	p.pay(req, req.amount)
	p.until(func() bool { return p.status(req) == "succeeded" }, "paid")
	p.stopWorkers[p.w]()
	_, err := p.w.pool.Exec(t.Context(), p.sql(`UPDATE $schema.solana_pay_references SET scan_stack = ARRAY['', $2] WHERE reference = $1`), req.reference, p.pay(req, 1))
	require.NoError(t, err)
	p.w.clock.Advance(8 * 24 * time.Hour)
	res, err := p.w.jobs.Insert(t.Context(), solanaPayGC{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	p.w.waitJob(res.Job.ID)
	require.Equal(t, "confirmed", p.referenceStatus(req), "mid-walk, the reference stays")
}

// Money is credited only once finalized. A relayed confirm waits a bounded
// time, then answers "processing" (the poller credits later); a transfer
// still at confirmed commitment credits nothing.
func TestSolanaPayCreditsOnlyFinalized(t *testing.T) {
	p := newSolanaPay(t)
	req := p.checkout(p.w.newCustomer())
	sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint,
		Amount: req.amount, Reference: req.reference, Memo: req.memo, Confirmed: true, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	session, err := p.w.engineConfirm(req.id, sig)
	require.NoError(t, err, "a transfer still confirming is not an error")
	require.Equal(t, "processing", session.Status)
	require.Equal(t, "requires_action", p.status(req), "confirmed is not credited")
	require.Empty(t, p.receiptOn(req, sig))

	p.fake.Finalize(sig)
	p.until(func() bool { return p.status(req) == "succeeded" }, "the poller credits it once final")
	session, err = p.w.engineConfirm(req.id, sig)
	require.NoError(t, err)
	require.Equal(t, "succeeded", session.Status)
}

// The amount is what the recipient received however it was moved; value in
// the wrong asset is recorded for review, never ignored.
func TestSolanaPayAmountIsWhatTheRecipientReceived(t *testing.T) {
	p := newSolanaPay(t)
	for name, shape := range map[string]func(*solanafake.Transfer){
		"split across instructions": func(tr *solanafake.Transfer) { tr.Split = 3 },
		"inside another program":    func(tr *solanafake.Transfer) { tr.Inner = true },
		"into an account it owns":   func(tr *solanafake.Transfer) { tr.Account = solanago.NewWallet().PublicKey() },
	} {
		req := p.checkout(p.w.newCustomer())
		tr := solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: req.recipient, Mint: req.mint, Amount: req.amount,
			Reference: req.reference, Memo: req.memo, BlockTime: p.w.clock.Now()}
		shape(&tr)
		sig, err := p.fake.Pay(tr)
		require.NoError(t, err)
		p.until(func() bool { return p.status(req) == "succeeded" }, name)
		require.Equal(t, "credited", p.receiptOn(req, sig), name)
	}

	wrong := p.checkout(p.w.newCustomer())
	sig, err := p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: wrong.recipient, Mint: solanafake.DevnetUSDCMint,
		Amount: wrong.amount, Reference: wrong.reference, Memo: wrong.memo, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	p.until(func() bool { return p.receiptOn(wrong, sig) != "" }, "the wrong asset is recorded")
	d, reason := p.receipt(sig)
	require.Equal(t, []string{"review", "wrong_asset"}, []string{d, reason})
	require.Equal(t, 0, p.payments(wrong))
}

// A failed attempt or a foreign transaction on the reference does not stop
// the buyer from getting a new transaction once the old one can no longer land.
func TestSolanaPayRetryAfterFailedAttempt(t *testing.T) {
	p := newSolanaPay(t)
	wallet := solanago.NewWallet().PublicKey()
	buyer := p.w.newCustomer()
	session, post := p.transactionRequest(buyer, "DUSD", wallet)
	status, first := post()
	require.Equal(t, http.StatusOK, status)
	terms := p.sessionTerms(buyer, session)
	_, err := p.fake.Pay(solanafake.Transfer{Payer: wallet, Recipient: terms.recipient, Mint: terms.mint, Amount: terms.amount,
		Reference: terms.reference, Memo: terms.memo, Failed: true, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	_, err = p.fake.Pay(solanafake.Transfer{Payer: solanago.NewWallet().PublicKey(), Recipient: terms.recipient, Mint: terms.mint, Amount: terms.amount,
		Reference: terms.reference, Memo: solanaint.PurchaseMemo(uuid.New()), BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	garbled, err := p.fake.Pay(solanafake.Transfer{Payer: wallet, Recipient: terms.recipient, Mint: terms.mint, Amount: terms.amount,
		Reference: terms.reference, Memo: terms.memo, Garbled: true, BlockTime: p.w.clock.Now()})
	require.NoError(t, err)
	p.fake.AdvanceBlocks(200)
	status, retry := post()
	require.Equal(t, http.StatusOK, status, "failed, foreign and unreadable transactions are not a payment in flight")
	require.NotEqual(t, first, retry)
	p.until(func() bool { return p.receiptOn(terms, garbled) != "" }, "the unreadable transaction is recorded")
	d, reason := p.receipt(garbled)
	require.Equal(t, []string{"review", "unreadable"}, []string{d, reason})
}

// Money awaiting a transfer, or received and not yet refunded, holds the
// billing archive back until an operator resolves it.
func TestSolanaPayHoldsTheBillingArchive(t *testing.T) {
	p := newSolanaPay(t)
	refusedBy := func() string {
		status, body := p.w.staff(http.MethodGet, "/v1/admin/billing-archive")
		if status == http.StatusOK {
			return ""
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table
	}
	req := p.checkout(p.w.newCustomer())
	// The session expiry job closes the checkout while its reference can still
	// be paid within the grace window.
	_, err := p.w.pool.Exec(t.Context(), p.sql(`UPDATE $schema.checkout_attempts SET status = 'expired' WHERE id = $1`), uuidOf(t, req.id))
	require.NoError(t, err)
	require.Equal(t, "solana_pay_references", refusedBy(), "a pending reference holds the archive")

	p.pay(req, req.amount)
	p.until(func() bool { return p.status(req) == "succeeded" }, "paid")
	second := p.pay(req, req.amount)
	p.until(func() bool { return p.receiptOn(req, second) == "review" }, "second transfer recorded")
	require.Equal(t, "solana_pay_receipts", refusedBy(), "money to refund holds the archive")

	var mid uuid.UUID
	require.NoError(t, p.w.pool.QueryRow(t.Context(), p.sql(`SELECT id FROM $schema.merchants WHERE slug = $1`), p.w.slug).Scan(&mid))
	require.NoError(t, hosttools.ResolveSolanaPayReview(t.Context(), engine.Graph(p.w.rt), billing.MerchantID(mid), second, "refunded in tx RefundSig"))
	require.NotEqual(t, "solana_pay_receipts", refusedBy(), "a resolved review no longer holds the archive")
}

// A Token-2022 transfer fee is priced in: the request asks for the gross that
// delivers the quote, the built transaction asserts the fee, and a fee raised
// after the quote leaves the merchant short, which is recorded for review.
func TestSolanaPayToken2022TransferFee(t *testing.T) {
	p := newSolanaPay(t)
	p.fake.SetTransferFee(solanafake.DevnetPYUSDMint, 100, 1_000_000_000) // 1%
	req := p.checkoutIn(p.w.newCustomer(), "PYUSD")
	net := p.sessionTerms(nil, req.id).amount
	require.Greater(t, req.amount, net, "the wallet is asked for the gross")
	sig := p.pay(req, req.amount)
	p.until(func() bool { return p.status(req) == "succeeded" }, "the net received settles the quote")
	d, reason := p.receipt(sig)
	require.Equal(t, []string{"credited", ""}, []string{d, reason})

	raised := p.checkoutIn(p.w.newCustomer(), "PYUSD")
	p.fake.SetTransferFee(solanafake.DevnetPYUSDMint, 200, 1_000_000_000)
	short := p.pay(raised, raised.amount)
	p.until(func() bool { return p.receiptOn(raised, short) != "" }, "the short transfer is recorded")
	d, reason = p.receipt(short)
	require.Equal(t, []string{"review", "underpaid"}, []string{d, reason})
	require.Equal(t, 0, p.payments(raised))

	session, post := p.transactionRequest(p.w.newCustomer(), "PYUSD", solanago.NewWallet().PublicKey())
	status, encoded := post()
	require.Equal(t, http.StatusOK, status)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(raw)
	require.NoError(t, err)
	data := []byte(tx.Message.Instructions[len(tx.Message.Instructions)-1].Data)
	quoted := p.sessionTerms(nil, session).amount
	fee := solanaint.TransferFee{MaximumFee: 1_000_000_000, BasisPts: 200}
	gross, wantFee, err := fee.GrossFor(quoted)
	require.NoError(t, err)
	require.Equal(t, []byte{26, 1}, data[:2], "TransferCheckedWithFee")
	require.Equal(t, gross, binary.LittleEndian.Uint64(data[2:]))
	require.Equal(t, wantFee, binary.LittleEndian.Uint64(data[11:]), "the fee is asserted on-chain")
}

// A mint with a transfer hook is refused for transaction requests at quote
// time; wallets building a transfer request resolve hooks themselves.
func TestSolanaPayRefusesTransferHookForTransactionRequest(t *testing.T) {
	p := newSolanaPay(t)
	p.fake.SetTransferHook(solanafake.DevnetPYUSDMint, solanago.NewWallet().PublicKey())
	_, err := p.w.engineCheckout(p.w.newCustomer(), p.transactionRequestFor("PYUSD"))
	require.ErrorContains(t, err, "transfer hook")
	req := p.checkoutIn(p.w.newCustomer(), "PYUSD")
	require.NotEmpty(t, req.reference)
}
