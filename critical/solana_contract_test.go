//go:build integration

package critical_test

import (
	"bytes"
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
	reads       map[string]int
	gate        *wireGate
	historyGate *wireGate
	signed      chan string
	aborted     chan struct{}
	failure     string
}

func newSolanaWire(t *testing.T, node *solanafake.Node) *solanaWire {
	t.Helper()
	u, err := url.Parse(node.URL())
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(u)
	w := &solanaWire{reads: map[string]int{}, signed: make(chan string, 1), aborted: make(chan struct{}, 1)}
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
		var historyGate *wireGate
		if rpc.Method == "sendTransaction" {
			w.sent++
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

type solanaFixtureOptions struct {
	freshWallet bool
	manualFleet bool
	network     string
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
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		w.cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{SolanaRPCURL: s.wire.server.URL}
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"solana": openrails.SolanaPSP{Environment: openrails.EnvironmentDevnet, Treasury: s.treasury.PublicKey().String(), RPCURL: s.wire.server.URL}.PSPConfig()}
		w.cfg.Merchants["alpha"] = m
		if recurring {
			w.cfg.Solana = &openrails.SolanaConfig{Crankers: []openrails.SolanaCrankerConfig{{Mode: "local_keypair", PrivateKey: s.cranker.PrivateKey.String()}}}
		}
	})
	s.server.Client().Timeout = 30 * time.Second
	if !recurring && !option.manualFleet {
		startFleet(t, s.world)
	}
	return s
}

func (s *solanaWorld) order(t *testing.T, recurring bool) billing.Order {
	t.Helper()
	ownership := catalog.OwnershipConsumable
	if recurring {
		ownership = catalog.OwnershipUnique
	}
	p, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-content", DisplayName: "Chain purchase", Ownership: ownership, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	priceInput := billing.CreatePriceParams{ProductID: p.ID, UnitAmount: 10_000_000, Currency: "USD", SellVia: []string{billing.SellViaSolana}}
	if recurring {
		priceInput.BillingIntervalHours = ptr(168)
		priceInput.AccessDurationHours = ptr(168)
	}
	price, err := s.alpha.CreatePrice(t.Context(), priceInput)
	require.NoError(t, err)
	status, raw := s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &billing.OrderPaymentParams{TokenSymbol: "USDC"}}, map[string]string{"Idempotency-Key": "solana-order"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.NotEqual(t, billing.OrderComplete, order.Status)
	require.NotNil(t, order.Payment.NextAction)
	require.Equal(t, "solana_pay", order.Payment.NextAction.Type)
	return order
}

func (s *solanaWorld) confirm(t *testing.T, order billing.Order, signature string) billing.Order {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/confirm", s.aliceToken, billing.ConfirmOrderParams{Signature: ptr(signature)})
	require.Equal(t, http.StatusOK, status, string(raw))
	var result billing.Order
	require.NoError(t, json.Unmarshal(raw, &result))
	require.Equal(t, order.ID, result.ID)
	return result
}

func (s *solanaWorld) noAccess(t *testing.T, order billing.Order) {
	t.Helper()
	current, err := s.alpha.GetOrder(t.Context(), order.ID)
	require.NoError(t, err)
	require.NotEqual(t, billing.OrderComplete, current.Status)
	access, err := s.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	require.False(t, access.Entitlements["chain:paid"])
	events, err := s.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
	require.NoError(t, err)
	require.Empty(t, events.Events)
}

func (s *solanaWorld) paid(t *testing.T, order billing.Order, signature string) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := s.alpha.GetOrder(t.Context(), order.ID)
		return err == nil && current.Status == billing.OrderComplete
	}, 20*time.Second, 20*time.Millisecond)
	payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(s.alice.ID))})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1)
	require.EqualValues(t, 10_000_000, payments.Items[0].Amount)
	require.Equal(t, signature, payments.Items[0].TransactionID)
	require.Equal(t, billing.PaymentSucceeded, payments.Items[0].Status)
	access, err := s.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: order.CustomerID, Entitlements: []string{"chain:paid"}})
	require.NoError(t, err)
	require.True(t, access.Entitlements["chain:paid"])
	events, err := s.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events.Events, 2)
}

func (s *solanaWorld) walletTransaction(t *testing.T, order billing.Order) *solanago.Transaction {
	t.Helper()
	status, raw := s.request(t, http.MethodPost, "/v1/orders/"+order.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": s.payer.PublicKey().String()})
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
	t.Fatal("the public wallet transaction must bind its order with a memo")
	return ""
}

func (s *solanaWorld) subscribe(t *testing.T) (billing.Order, *billing.Subscription) {
	t.Helper()
	order := s.order(t, true)
	tx := s.walletTransaction(t, order)
	sig, err := s.node.Land(tx, s.clock.Now())
	require.NoError(t, err)
	s.confirm(t, order, sig.String())
	s.paid(t, order, sig.String())
	subs, err := s.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: order.CustomerID})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	sub, err := s.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.Equal(t, "solana", sub.Rail)
	require.Equal(t, s.clock.Now().Add(168*time.Hour), *sub.CurrentPeriodEndsAt)
	require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	require.EqualValues(t, 90_000_000, s.node.Balance(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	return order, sub
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
	facts, err := s.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, facts.Events, 2)
}

func TestSolanaContracts(t *testing.T) {
	t.Run("OR-J37-S02/signed-pull-survives-process-death-before-network-submission", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		s.clock.Advance(due.Sub(s.clock.Now()))
		queue := hostFleet(t, s.world)
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		defer gate.unblock()
		s.wire.mu.Lock()
		s.wire.gate = gate
		s.wire.mu.Unlock()
		beforeSends := s.wire.sends()
		child := startWorkerProcess(t, s.world, nil)
		_, err := queue.Insert(t.Context(), registeredJob{KindName: "openrails.solana_crank"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
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
		order, sub := s.subscribe(t)
		paid, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		for range 2 {
			s.confirm(t, order, paid.Items[0].TransactionID)
		}
		due := *sub.CurrentPeriodEndsAt
		s.clock.Advance(due.Sub(s.clock.Now()))
		fleet := startFleet(t, s.world)
		runRegistered(t, fleet, "openrails.solana_crank", 2)
		s.renewed(t, sub, due.Add(168*time.Hour))
		runRegistered(t, fleet, "openrails.solana_crank", 2)
		s.renewed(t, sub, due.Add(168*time.Hour))
	})
	t.Run("OR-J37-S05", func(t *testing.T) {
		t.Run("wrong-chain-identity", func(t *testing.T) {
			s := newSolanaWorld(t, false, solanaFixtureOptions{network: "mainnet"})
			product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "wrong-chain", DisplayName: "Wrong chain"})
			require.NoError(t, err)
			price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
			require.NoError(t, err)
			status, raw := s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &billing.OrderPaymentParams{TokenSymbol: "USDC"}}, map[string]string{"Idempotency-Key": "wrong-chain-purchase"})
			require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
			require.Contains(t, string(raw), `"code":"payment_option_unavailable"`)
			payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: billing.CustomerID(uuid.MustParse(s.alice.ID))})
			require.NoError(t, err)
			require.Empty(t, payments.Items)
			require.Zero(t, s.wire.sends())
		})
		t.Run("foreign-wallet-and-unrelated-instructions", func(t *testing.T) {
			s := newSolanaWorld(t, true)
			order := s.order(t, true)
			status, raw := s.request(t, http.MethodPost, "/v1/orders/"+order.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": "invalid-wallet"})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			s.walletTransaction(t, order)
			foreign := solanago.NewWallet().PublicKey()
			status, raw = s.request(t, http.MethodPost, "/v1/orders/"+order.ID.String()+"/solana-pay", s.aliceToken, map[string]string{"account": foreign.String()})
			require.Equal(t, http.StatusConflict, status, string(raw))
			sig, err := s.node.Pay(solanafake.Transfer{Payer: foreign, Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000, Reference: order.Payment.NextAction.Payload["reference"], BlockTime: s.clock.Now()})
			require.NoError(t, err)
			status, raw = s.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/confirm", s.aliceToken, billing.ConfirmOrderParams{Signature: ptr(sig)})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			s.noAccess(t, order)
			payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: order.CustomerID})
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
				s.clock.Advance(sub.CurrentPeriodEndsAt.Sub(s.clock.Now()))
				before := s.wire.sends()
				fleet := startFleet(t, s.world)
				runRegistered(t, fleet, "openrails.solana_crank", 2)
				require.Greater(t, s.wire.sends(), before, "the explicit external RPC refusal must actually occur")
				current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
				payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
				require.NoError(t, err)
				require.Len(t, payments.Items, 1)
				require.EqualValues(t, 10_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
				require.EqualValues(t, 90_000_000, s.node.Balance(s.payer.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
				facts, err := s.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
				require.NoError(t, err)
				require.Len(t, facts.Events, 1)
			})
		}
	})
	t.Run("OR-J37-S01/foreign-or-underpaid-transfer-cannot-settle-the-order", func(t *testing.T) {
		s := newSolanaWorld(t, false, solanaFixtureOptions{manualFleet: true})
		order := s.order(t, false)
		prepared := s.walletTransaction(t, order)
		action := order.Payment.NextAction
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
				s.confirm(t, order, sig)
				s.noAccess(t, order)
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
		s.confirm(t, order, sig)
		gate.unblock()
		require.Eventually(t, func() bool {
			for _, invalid := range invalidSignatures {
				if !s.wire.read(invalid) {
					return false
				}
			}
			return true
		}, 10*time.Second, 20*time.Millisecond)
		s.noAccess(t, order)
		// The in-flight history response did not contain the later payment.
		// Let the injected business clock advance through normal rechecks;
		// waiting in wall time alone leaves the next poll permanently in future.
		require.Eventually(t, func() bool {
			s.clock.Advance(time.Second)
			current, err := s.alpha.GetOrder(t.Context(), order.ID)
			return err == nil && current.Status == billing.OrderComplete
		}, 20*time.Second, 100*time.Millisecond)
		s.paid(t, order, sig)
	})
	t.Run("OR-J37-S03/unknown-reverted-and-unfinalized-transfers-do-not-create-money", func(t *testing.T) {
		s := newSolanaWorld(t, false)
		order := s.order(t, false)
		prepared := s.walletTransaction(t, order)
		unknown, err := s.payer.PrivateKey.Sign([]byte("unknown chain transaction"))
		require.NoError(t, err)
		s.confirm(t, order, unknown.String())
		s.noAccess(t, order)
		transfer := solanafake.Transfer{Payer: s.payer.PublicKey(), Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000, Reference: order.Payment.NextAction.Payload["reference"], BlockTime: s.clock.Now(), Failed: true, Memo: solanaMemo(t, prepared)}
		reverted, err := s.node.Pay(transfer)
		require.NoError(t, err)
		s.confirm(t, order, reverted)
		s.noAccess(t, order)
		transfer.Failed, transfer.Confirmed = false, true
		beforeReads := s.wire.historyReads(transfer.Reference)
		sig, err := s.node.Pay(transfer)
		require.NoError(t, err)
		s.clock.Advance(4 * time.Second)
		s.confirm(t, order, sig)
		require.Eventually(t, func() bool { return s.wire.historyReads(transfer.Reference) > beforeReads }, 10*time.Second, 20*time.Millisecond)
		require.Never(t, func() bool {
			current, err := s.alpha.GetOrder(t.Context(), order.ID)
			return err == nil && current.Status == billing.OrderComplete
		}, 1200*time.Millisecond, 30*time.Millisecond, "a merely confirmed transfer must not settle before finality")
		s.noAccess(t, order)
		s.node.Finalize(sig)
		s.clock.Advance(4 * time.Second)
		for range 2 {
			s.confirm(t, order, sig)
		}
		s.paid(t, order, sig)
	})
}
