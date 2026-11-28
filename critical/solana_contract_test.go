//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// The Solana node is the external chain boundary. Its account/token balances
// and signatures remain independent from the real OpenRails database.
type solanaWorld struct {
	*authorityWorld
	node                     *solanafake.Node
	treasury, payer, cranker *solanago.Wallet
	wire                     *solanaWire
}

type solanaWire struct {
	server      *httptest.Server
	mu          sync.Mutex
	sent        int
	lastSent    string
	reads       map[string]int
	gate        *wireGate
	historyGate *wireGate
	signed      chan string
	aborted     chan struct{}
	failure     string
	// failMethod refuses every call of one RPC method; calls counts them all.
	failMethod string
	calls      map[string]int
}

func newSolanaWire(t *testing.T, node *solanafake.Node) *solanaWire {
	t.Helper()
	u, err := url.Parse(node.URL())
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(u)
	w := &solanaWire{reads: map[string]int{}, calls: map[string]int{}, signed: make(chan string, 1), aborted: make(chan struct{}, 1)}
	w.server = httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(out, err.Error(), 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var rpc struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.Unmarshal(data, &rpc) != nil {
			http.Error(out, "invalid RPC fixture input", 400)
			return
		}
		w.mu.Lock()
		gate, failure := w.gate, w.failure
		w.calls[rpc.Method]++
		refused := rpc.Method == w.failMethod
		var historyGate *wireGate
		if rpc.Method == "sendTransaction" {
			w.sent++
			var encoded string
			if len(rpc.Params) > 0 && json.Unmarshal(rpc.Params[0], &encoded) == nil {
				if tx, err := solanago.TransactionFromBase64(encoded); err == nil && len(tx.Signatures) > 0 {
					w.lastSent = tx.Signatures[0].String()
				}
			}
		}
		if rpc.Method == "getTransaction" && len(rpc.Params) > 0 {
			var sig string
			_ = json.Unmarshal(rpc.Params[0], &sig)
			w.reads[sig]++
		}
		if rpc.Method == "getSignaturesForAddress" && len(rpc.Params) > 0 {
			var reference string
			_ = json.Unmarshal(rpc.Params[0], &reference)
			w.reads["reference:"+reference]++
			historyGate, w.historyGate = w.historyGate, nil
		}
		if rpc.Method == "sendTransaction" && gate != nil {
			w.gate = nil
		}
		w.mu.Unlock()
		if refused {
			out.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32005, "message": "Node is behind"}})
			return
		}
		if historyGate != nil {
			// Capture the node's history before admitting a later transfer, but
			// delay delivery to reproduce an in-flight poll finishing afterward.
			response := httptest.NewRecorder()
			proxy.ServeHTTP(response, r)
			historyGate.once.Do(func() { close(historyGate.arrived) })
			select {
			case <-historyGate.release:
			case <-r.Context().Done():
				return
			}
			for key, values := range response.Header() {
				out.Header()[key] = values
			}
			out.WriteHeader(response.Code)
			_, _ = out.Write(response.Body.Bytes())
			return
		}
		if rpc.Method == "sendTransaction" {
			if gate != nil {
				var signed string
				_ = json.Unmarshal(rpc.Params[0], &signed)
				w.signed <- signed
				gate.once.Do(func() { close(gate.arrived) })
				select {
				case <-gate.release:
				case <-r.Context().Done():
					w.aborted <- struct{}{}
					return
				}
			}
			if failure != "" {
				out.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(out).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32002, "message": "Transaction simulation failed: " + failure, "data": map[string]any{"err": failure}}})
				return
			}
		}
		proxy.ServeHTTP(out, r)
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *solanaWire) readCount(signature string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads[signature]
}

func (w *solanaWire) read(signature string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads[signature] > 0
}
func (w *solanaWire) historyReads(reference string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reads["reference:"+reference]
}
func (w *solanaWire) sends() int { w.mu.Lock(); defer w.mu.Unlock(); return w.sent }

// lastSignature is the signature of the last transaction sent.
func (w *solanaWire) lastSignature() string { w.mu.Lock(); defer w.mu.Unlock(); return w.lastSent }
func (w *solanaWire) count(method string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[method]
}
func (w *solanaWire) refuse(method string) { w.mu.Lock(); defer w.mu.Unlock(); w.failMethod = method }

type solanaFixtureOptions struct {
	freshWallet bool
	manualFleet bool
	network     string
	// fx replaces the fixed FX wire.
	fx http.RoundTripper
	// dunning is the merchant's dunning policy.
	dunning *billing.DunningPolicy
	// alerts receives the merchant's operator alerts.
	alerts *alertMailbox
}

func newSolanaWorld(t *testing.T, recurring bool, options ...solanaFixtureOptions) *solanaWorld {
	t.Helper()
	s := &solanaWorld{node: solanafake.New(), treasury: solanago.NewWallet(), payer: solanago.NewWallet(), cranker: solanago.NewWallet()}
	t.Cleanup(s.node.Close)
	var option solanaFixtureOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.network != "" {
		s.node.SetNetwork(option.network)
	}
	s.wire = newSolanaWire(t, s.node)
	s.node.SetSOLBalance(s.cranker.PublicKey(), 10_000_000_000)
	s.node.SetSOLBalance(s.payer.PublicKey(), 10_000_000_000)
	s.node.Fund(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint), 100_000_000)
	if recurring && !option.freshWallet {
		// A returning wallet already has its on-chain delegation authority.
		// The duplicate-confirmation contract separately covers a first wallet.
		require.NoError(t, s.node.Authority(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint), 123))
	}
	s.authorityWorld = newAuthorityWorld(t, func(w *world) {
		// Keep every simulated renewal in the real database's past: its money
		// schema independently rejects future provider receipts.
		w.clock.Advance(-30 * 24 * time.Hour)
		w.clock.Advance(w.clock.Now().Truncate(time.Second).Sub(w.clock.Now()))
		s.node.SetClock(w.clock.Now)
		w.posture = "full"
		w.deps.ProviderEndpoints = openrails.ProviderEndpoints{SolanaRPCURL: s.wire.server.URL}
		if option.fx != nil {
			w.deps.FXTransport = option.fx
		}
		m := cfgMerchant(w.cfg, "alpha")
		m.PSPs = billing.PSPs{"solana": openrails.SolanaPSP{Environment: billing.EnvironmentDevnet, Treasury: s.treasury.PublicKey().String(), RPCURL: s.wire.server.URL}.PSPConfig()}
		if option.dunning != nil {
			m.MerchantSettings.DunningPolicy = option.dunning
		}
		if option.alerts != nil {
			m.AlertEmail = ptr("ops@alpha.example")
			w.deps.Email = option.alerts
		}
		setCfgMerchant(&w.cfg, "alpha", m)
		if recurring {
			w.cfg.Solana = billing.SolanaConfig{Crankers: []billing.SolanaCrankerConfig{{Mode: "local_keypair", PrivateKey: s.cranker.PrivateKey.String()}}}
		}
	})
	s.server.Client().Timeout = 30 * time.Second
	if !recurring && !option.manualFleet {
		startFleet(t, s.world)
	}
	return s
}

func (s *solanaWorld) invoice(t *testing.T, recurring bool) billing.Invoice {
	t.Helper()
	ownership := catalog.OwnershipConsumable
	if recurring {
		ownership = catalog.OwnershipUnique
	}
	p, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-content", DisplayName: "Chain purchase", Ownership: ownership, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	priceInput := billing.CreatePriceParams{ProductID: p.ID, UnitAmount: 10_000_000, Currency: "USD", SellVia: []catalog.SellVia{catalog.SellViaSolana}}
	if recurring {
		priceInput.BillingIntervalHours = ptr(168)
		priceInput.AccessDurationHours = ptr(168)
	}
	price, err := s.alpha.CreatePrice(t.Context(), priceInput)
	require.NoError(t, err)
	status, raw := s.request(t, http.MethodPost, "/v1/me/invoices", s.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &billing.InvoicePaymentParams{TokenSymbol: "USDC"}}, map[string]string{"Idempotency-Key": "solana-invoice"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.NotEqual(t, billing.InvoiceComplete, invoice.Status)
	require.NotNil(t, invoice.Payment.NextAction)
	require.Equal(t, "solana_pay", invoice.Payment.NextAction.Type)
	return invoice
}

func (s *solanaWorld) confirm(t *testing.T, invoice billing.Invoice, signature string) billing.Invoice {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/confirm", s.aliceToken, billing.ConfirmInvoiceParams{Signature: ptr(signature)})
	require.Equal(t, http.StatusOK, status, string(raw))
	var result billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, invoice.ID, result.ID)
	return result
}

func (s *solanaWorld) noAccess(t *testing.T, invoice billing.Invoice) {
	t.Helper()
	current, err := s.alpha.GetInvoice(t.Context(), invoice.ID)
	require.NoError(t, err)
	require.NotEqual(t, billing.InvoiceComplete, current.Status)
	access, err := s.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: invoice.CustomerID, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	require.False(t, access.Entitlements["chain:paid"])
	events, err := s.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled", "invoice.completed"}, Limit: 100})
	require.NoError(t, err)
	require.Empty(t, events.Events)
}

func (s *solanaWorld) paid(t *testing.T, invoice billing.Invoice, signature string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := s.alpha.GetInvoice(t.Context(), invoice.ID)
		return err == nil && current.Status == billing.InvoiceComplete
	}, 20*time.Second, 20*time.Millisecond)
	payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(s.alice.ID))})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1)
	require.EqualValues(t, 10_000_000, payments.Items[0].Amount)
	require.Equal(t, signature, payments.Items[0].TransactionID)
	require.Equal(t, billing.PaymentSucceeded, payments.Items[0].Status)
	access, err := s.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: invoice.CustomerID, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	require.True(t, access.Entitlements["chain:paid"])
	events, err := s.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled", "invoice.completed"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events.Events, 2)
}

func (s *solanaWorld) walletTransaction(t *testing.T, invoice billing.Invoice) *solanago.Transaction {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/invoices/"+invoice.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": s.payer.PublicKey().String()})
	require.Equal(t, http.StatusOK, status, string(raw))
	var prepared struct {
		Transaction string `json:"transaction"`
	}
	require.NoError(t, json.Unmarshal(raw, &prepared))
	tx, err := solanago.TransactionFromBase64(prepared.Transaction)
	require.NoError(t, err)
	_, err = tx.PartialSign(func(key solanago.PublicKey) *solanago.PrivateKey {
		if key.Equals(s.payer.PublicKey()) {
			return &s.payer.PrivateKey
		}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, tx.VerifySignatures())
	return tx
}

func solanaMemo(t *testing.T, tx *solanago.Transaction) string {
	t.Helper()
	for _, instruction := range tx.Message.Instructions {
		if tx.Message.AccountKeys[instruction.ProgramIDIndex].Equals(solanago.MemoProgramID) {
			return string(instruction.Data)
		}
	}
	t.Fatal("the public wallet transaction must bind its invoice with a memo")
	return ""
}

func (s *solanaWorld) subscribe(t *testing.T) (billing.Invoice, *billing.Subscription) {
	t.Helper()
	invoice := s.invoice(t, true)
	tx := s.walletTransaction(t, invoice)
	sig, err := s.node.Land(tx, s.clock.Now())
	require.NoError(t, err)
	s.confirm(t, invoice, sig.String())
	s.paid(t, invoice, sig.String())
	subs, err := s.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: invoice.CustomerID})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	sub, err := s.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.Equal(t, billing.RailSolana, sub.Rail)
	require.Equal(t, s.clock.Now().Add(168*time.Hour), *sub.CurrentPeriodEndsAt)
	require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	require.EqualValues(t, 90_000_000, s.node.Balance(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	return invoice, sub
}

func (s *solanaWorld) renewed(t *testing.T, sub *billing.Subscription, end time.Time) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		return err == nil && current.CurrentPeriodEndsAt != nil && current.CurrentPeriodEndsAt.Equal(end)
	}, 20*time.Second, 20*time.Millisecond)
	payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
	require.NoError(t, err)
	require.Len(t, payments.Items, 2)
	for _, payment := range payments.Items {
		require.EqualValues(t, 10_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
	}
	require.EqualValues(t, 20_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	require.EqualValues(t, 80_000_000, s.node.Balance(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	facts, err := s.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, facts.Events, 2)
}

func TestSolanaContracts(t *testing.T) {
	t.Run("OR-J37-S75/an-unpaid-reference-is-read-less-often-as-it-ages", func(t *testing.T) {
		s := newSolanaWorld(t, false, solanaFixtureOptions{manualFleet: true})
		invoice := s.invoice(t, false)
		s.walletTransaction(t, invoice)
		reference := invoice.Payment.NextAction.Payload["reference"]
		table := pgx.Identifier{s.schema, "solana_pay_references"}.Sanitize()
		var created, next time.Time
		require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT created_at, next_poll_at FROM "+table+" WHERE reference = $1", reference).Scan(&created, &next))
		require.WithinDuration(t, created, next, 3*time.Second)
		startFleet(t, s.world)
		for _, step := range []struct {
			age, interval time.Duration
		}{{0, 3 * time.Second}, {2 * time.Minute, 12 * time.Second}, {10 * time.Minute, 30 * time.Second}} {
			at := created.Add(step.age)
			reads := s.wire.historyReads(reference)
			if step.age > 0 {
				s.clock.Advance(at.Sub(s.clock.Now()))
			} else {
				reads = 0
			}
			require.Eventually(t, func() bool {
				if s.clock.Now().Before(at.Add(time.Second)) {
					s.clock.Advance(100 * time.Millisecond)
				}
				err := s.pool.QueryRow(t.Context(), "SELECT next_poll_at FROM "+table+" WHERE reference = $1", reference).Scan(&next)
				return err == nil && s.wire.historyReads(reference) > reads && next.After(s.clock.Now()) && next.Before(at.Add(45*time.Second))
			}, 10*time.Second, 20*time.Millisecond)
			require.WithinDuration(t, at.Add(step.interval), next, 2*time.Second, "reference age %s", step.age)
			if step.age == 0 {
				require.LessOrEqual(t, next.Sub(s.clock.Now()), 3*time.Second)
			}
		}
	})
	t.Run("OR-J37-S02/signed-pull-survives-process-death-before-network-submission", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		// The period's pull is due its margin after the period end.
		s.clock.Advance(due.Add(time.Minute).Sub(s.clock.Now()))
		queue := hostFleet(t, s.world)
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		defer gate.unblock()
		s.wire.mu.Lock()
		s.wire.gate = gate
		s.wire.mu.Unlock()
		beforeSends := s.wire.sends()
		child := startWorkerProcess(t, s.world, nil)
		select {
		case <-gate.arrived:
		case <-time.After(20 * time.Second):
			t.Fatal("signed pull did not reach the external send barrier")
		}
		signed := <-s.wire.signed
		tx, err := solanago.TransactionFromBase64(signed)
		require.NoError(t, err)
		require.NoError(t, tx.VerifySignatures())
		sig := tx.Signatures[0].String()
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs)
		child.kill(t)
		select {
		case <-s.wire.aborted:
		case <-time.After(5 * time.Second):
			t.Fatal("the killed process did not abandon its unsent RPC")
		}
		gate.unblock()
		require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
		s.clock.Advance(20 * time.Minute)
		replacement := startWorkerProcess(t, s.world, nil)
		for _, job := range jobs.Jobs {
			_, err := queue.Insert(t.Context(), replayedOperation{Payload: job.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
			require.NoError(t, err)
		}
		require.Eventually(t, func() bool { return s.wire.read(sig) }, 20*time.Second, 20*time.Millisecond, "replacement must know the exact signature written before the original send")
		// The read is seen on arrival. The verdict schedules the next read from
		// the clock at the instant it is recorded, so it is recorded before the
		// transaction lands and the clock moves.
		operations := pgx.Identifier{s.schema, "provider_intents"}.Sanitize()
		t.Cleanup(func() {
			if !t.Failed() {
				return
			}
			var state string
			err := s.pool.QueryRow(context.Background(), "SELECT format('status=%s attempts=%s next_attempt_at=%s lease_expires_at=%s reason=%s', status, attempts, next_attempt_at, lease_expires_at, last_failure_reason) FROM "+operations+" WHERE intent_type = 'solana_pull' ORDER BY created_at, id LIMIT 1").Scan(&state)
			t.Logf("pull operation at %s: %s (%v)", s.clock.Now().UTC().Format(time.RFC3339), state, err)
			wakes, err := queue.JobList(context.Background(), river.NewJobListParams().Kinds("openrails.provider_operation").First(100))
			if err != nil {
				t.Logf("pull operation jobs: %v", err)
				return
			}
			for _, job := range wakes.Jobs {
				t.Logf("pull operation job %d: state=%s attempt=%d scheduled_at=%s", job.ID, job.State, job.Attempt, job.ScheduledAt.UTC().Format(time.RFC3339))
			}
		})
		require.Eventually(t, func() bool {
			var unresolved bool
			err := s.pool.QueryRow(t.Context(), "SELECT status = 'unknown_needs_verify' AND lease_expires_at IS NULL AND last_failure_reason = 'recorded pull signature still unresolved' FROM "+operations+" WHERE intent_type = 'solana_pull' ORDER BY created_at, id LIMIT 1").Scan(&unresolved)
			return err == nil && unresolved
		}, 20*time.Second, 20*time.Millisecond, "replacement must record the unresolved signature before it lands")
		require.Equal(t, beforeSends+1, s.wire.sends(), "unknown signature is not permission to submit another pull")
		landedAt := s.clock.Now()
		landed, err := s.node.Land(tx, landedAt)
		require.NoError(t, err)
		require.Equal(t, sig, landed.String())
		s.clock.Advance(20 * time.Minute)
		replacement.advance(t, s.clock.Now())
		for _, job := range jobs.Jobs {
			for range 2 {
				_, err := queue.Insert(t.Context(), replayedOperation{Payload: job.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
			}
		}
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		var recorded bool
		for _, payment := range payments.Items {
			recorded = recorded || payment.TransactionID == sig
		}
		require.True(t, recorded)
		require.Equal(t, beforeSends+1, s.wire.sends())
		replacement.kill(t)
	})
	t.Run("OR-J37-S04/duplicate-confirmations-and-workers-cannot-pull-a-period-twice", func(t *testing.T) {
		s := newSolanaWorld(t, true, solanaFixtureOptions{freshWallet: true})
		invoice, sub := s.subscribe(t)
		paid, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		for range 2 {
			s.confirm(t, invoice, paid.Items[0].TransactionID)
		}
		due := *sub.CurrentPeriodEndsAt
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		fleet := startFleet(t, s.world)
		// Two wakes of the period's pull run at once.
		for _, job := range operationJobs(t, fleet) {
			_, err := fleet.Insert(t.Context(), replayedOperation{Payload: job.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
			require.NoError(t, err)
		}
		wakeOperations(t, fleet)
		s.renewed(t, sub, due.Add(pullMargin).Add(168*time.Hour))
		settleOperations(t, fleet)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		s.renewed(t, sub, due.Add(pullMargin).Add(168*time.Hour))
	})
	t.Run("OR-J37-S63/a-wallet-that-is-the-plans-cranker-is-refused-a-co-signed-subscribe", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		usdc := solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)
		// Funded and delegated, so only the cranker identity can refuse it.
		s.node.Fund(s.cranker.PublicKey(), usdc, 100_000_000)
		require.NoError(t, s.node.Authority(s.cranker.PublicKey(), usdc, 123))
		invoice := s.invoice(t, true)
		sends := s.wire.sends()
		status, raw := s.request(t, http.MethodPost, "/v1/invoices/"+invoice.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": s.cranker.PublicKey().String()})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Contains(t, string(raw), `"code":"invalid_param"`)
		var prepared struct {
			Transaction string `json:"transaction"`
		}
		require.NoError(t, json.Unmarshal(raw, &prepared))
		require.Empty(t, prepared.Transaction, "the cranker signs no transaction for its own wallet")
		require.Equal(t, sends, s.wire.sends())
		require.EqualValues(t, 100_000_000, s.node.Balance(s.cranker.PublicKey(), usdc))
		s.noAccess(t, invoice)
	})
	t.Run("OR-J37-S05", func(t *testing.T) {
		t.Run("wrong-chain-identity", func(t *testing.T) {
			s := newSolanaWorld(t, false, solanaFixtureOptions{network: "mainnet"})
			product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "wrong-chain", DisplayName: "Wrong chain"})
			require.NoError(t, err)
			price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
			require.NoError(t, err)
			status, raw := s.request(t, http.MethodPost, "/v1/me/invoices", s.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &billing.InvoicePaymentParams{TokenSymbol: "USDC"}}, map[string]string{"Idempotency-Key": "wrong-chain-purchase"})
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			require.Contains(t, string(raw), `"code":"payment_option_unavailable"`)
			payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(s.alice.ID))})
			require.NoError(t, err)
			require.Empty(t, payments.Items)
			require.Zero(t, s.wire.sends())
		})
		t.Run("foreign-wallet-and-unrelated-instructions", func(t *testing.T) {
			s := newSolanaWorld(t, true)
			invoice := s.invoice(t, true)
			status, raw := s.request(t, http.MethodPost, "/v1/invoices/"+invoice.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": "invalid-wallet"})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			s.walletTransaction(t, invoice)
			foreign := solanago.NewWallet().PublicKey()
			status, raw = s.request(t, http.MethodPost, "/v1/invoices/"+invoice.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": foreign.String()})
			require.Equal(t, http.StatusConflict, status, string(raw))
			sig, err := s.node.Pay(solanafake.Transfer{Payer: foreign, Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000, Reference: invoice.Payment.NextAction.Payload["reference"], BlockTime: s.clock.Now()})
			require.NoError(t, err)
			status, raw = s.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/confirm", s.aliceToken, billing.ConfirmInvoiceParams{Signature: ptr(sig)})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			s.noAccess(t, invoice)
			payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: invoice.CustomerID})
			require.NoError(t, err)
			require.Empty(t, payments.Items)
		})
		for _, fault := range []string{"BlockhashNotFound", "insufficient funds for fee"} {
			t.Run(fault, func(t *testing.T) {
				s := newSolanaWorld(t, true)
				_, sub := s.subscribe(t)
				s.wire.mu.Lock()
				s.wire.failure = fault
				s.wire.mu.Unlock()
				s.clock.Advance(sub.CurrentPeriodEndsAt.Add(pullMargin).Sub(s.clock.Now()))
				before := s.wire.sends()
				fleet := startFleet(t, s.world)
				wakeOperations(t, fleet)
				require.Eventually(t, func() bool { return s.wire.sends() > before }, 20*time.Second, 20*time.Millisecond, "the explicit external RPC refusal must actually occur")
				settleOperations(t, fleet)
				current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
				require.NoError(t, err)
				require.Len(t, payments.Items, 1)
				require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
				require.EqualValues(t, 90_000_000, s.node.Balance(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
				facts, err := s.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled"}, Limit: 100})
				require.NoError(t, err)
				require.Len(t, facts.Events, 1)
			})
		}
	})
	t.Run("OR-J37-S01/foreign-or-underpaid-transfer-cannot-settle-the-invoice", func(t *testing.T) {
		s := newSolanaWorld(t, false, solanaFixtureOptions{manualFleet: true})
		invoice := s.invoice(t, false)
		prepared := s.walletTransaction(t, invoice)
		action := invoice.Payment.NextAction
		require.Equal(t, solanafake.DevnetUSDCMint, action.Payload["token_mint"])
		require.Equal(t, s.treasury.PublicKey().String(), action.Payload["recipient"])
		require.Equal(t, "10.000000", action.Payload["token_amount"])
		ref := action.Payload["reference"]
		require.NotEmpty(t, ref)
		base := solanafake.Transfer{Payer: s.payer.PublicKey(), Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000, Reference: ref, BlockTime: s.clock.Now(), Memo: solanaMemo(t, prepared)}
		var invalidSignatures []string
		for _, profile := range []string{"foreign-treasury", "wrong-mint", "underpaid"} {
			t.Run(profile, func(t *testing.T) {
				bad := base
				switch profile {
				case "foreign-treasury":
					bad.Recipient = solanago.NewWallet().PublicKey().String()
				case "wrong-mint":
					bad.Mint = solanafake.DevnetSOLMint
				case "underpaid":
					bad.Amount = 9_000_000
				}
				sig, err := s.node.Pay(bad)
				require.NoError(t, err)
				invalidSignatures = append(invalidSignatures, sig)
				s.confirm(t, invoice, sig)
				s.noAccess(t, invoice)
			})
		}
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		defer gate.unblock()
		s.wire.mu.Lock()
		s.wire.historyGate = gate
		s.wire.mu.Unlock()
		startFleet(t, s.world)
		select {
		case <-gate.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the poller never read the invalid transfers")
		}
		sig, err := s.node.Pay(base)
		require.NoError(t, err)
		s.confirm(t, invoice, sig)
		gate.unblock()
		require.Eventually(t, func() bool {
			for _, invalid := range invalidSignatures {
				if !s.wire.read(invalid) {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond)
		s.noAccess(t, invoice)
		// The in-flight history response did not contain the later payment.
		// Let the injected business clock advance through normal rechecks;
		// waiting in wall time alone leaves the next poll permanently in future.
		require.Eventually(t, func() bool {
			s.clock.Advance(time.Second)
			current, err := s.alpha.GetInvoice(t.Context(), invoice.ID)
			return err == nil && current.Status == billing.InvoiceComplete
		}, 20*time.Second, 100*time.Millisecond)
		s.paid(t, invoice, sig)
	})
	t.Run("OR-J37-S03/unknown-reverted-and-unfinalized-transfers-do-not-create-money", func(t *testing.T) {
		s := newSolanaWorld(t, false)
		invoice := s.invoice(t, false)
		prepared := s.walletTransaction(t, invoice)
		unknown, err := s.payer.PrivateKey.Sign([]byte("unknown chain transaction"))
		require.NoError(t, err)
		s.confirm(t, invoice, unknown.String())
		s.noAccess(t, invoice)
		transfer := solanafake.Transfer{Payer: s.payer.PublicKey(), Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000, Reference: invoice.Payment.NextAction.Payload["reference"], BlockTime: s.clock.Now(), Failed: true, Memo: solanaMemo(t, prepared)}
		reverted, err := s.node.Pay(transfer)
		require.NoError(t, err)
		s.confirm(t, invoice, reverted)
		s.noAccess(t, invoice)
		transfer.Failed, transfer.Confirmed = false, true
		beforeReads := s.wire.historyReads(transfer.Reference)
		sig, err := s.node.Pay(transfer)
		require.NoError(t, err)
		s.clock.Advance(4 * time.Second)
		s.confirm(t, invoice, sig)
		require.Eventually(t, func() bool { return s.wire.historyReads(transfer.Reference) > beforeReads }, 10*time.Second, 20*time.Millisecond)
		require.Never(t, func() bool {
			current, err := s.alpha.GetInvoice(t.Context(), invoice.ID)
			return err == nil && current.Status == billing.InvoiceComplete
		}, 1200*time.Millisecond, 30*time.Millisecond, "a merely confirmed transfer must not settle before finality")
		s.noAccess(t, invoice)
		s.node.Finalize(sig)
		s.clock.Advance(4 * time.Second)
		for range 2 {
			s.confirm(t, invoice, sig)
		}
		s.paid(t, invoice, sig)
	})
	t.Run("OR-J37-S73/a-first-subscription-payment-confirmed-and-not-finalized-enrolls-nothing-until-it-finalizes", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		invoice := s.invoice(t, true)
		tx := s.walletTransaction(t, invoice)
		sig, err := s.node.LandConfirmed(tx, s.clock.Now())
		require.NoError(t, err)
		sends := s.wire.sends()
		pending := s.confirm(t, invoice, sig.String())
		require.NotEqual(t, billing.InvoiceComplete, pending.Status, "a confirmed transaction is landed, its settlement pending")
		s.noAccess(t, invoice)
		subs, err := s.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: invoice.CustomerID})
		require.NoError(t, err)
		require.Empty(t, subs.Items, "nothing is enrolled before finality")
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: invoice.CustomerID})
		require.NoError(t, err)
		require.Empty(t, payments.Items)
		require.Equal(t, sends, s.wire.sends(), "the server sends nothing for a pending first payment")

		s.node.Finalize(sig.String())
		s.confirm(t, invoice, sig.String())
		s.paid(t, invoice, sig.String())
		subs, err = s.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: invoice.CustomerID})
		require.NoError(t, err)
		require.Len(t, subs.Items, 1)
		require.Equal(t, billing.SubscriptionActive, subs.Items[0].Status)
		require.Equal(t, sends, s.wire.sends())
	})
}
