//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// The existing Stripe wire fixture models the external protocol, not an
// OpenRails service. Its economic effects are committed independently in this
// fixture-owned PostgreSQL journal before the response reaches the application.
// Application restarts never delete/reinitialize this journal or provider.
type providerLedger struct {
	secret              string
	core                *stripemock.Mock
	server              *httptest.Server
	pool                *pgxpool.Pool
	schema              string
	mu                  sync.Mutex
	lostResponse        bool
	lostRefundResponse  bool
	evidenceUnavailable bool
	evidenceRequested   chan struct{}
	responseGate        *wireGate
	errors              []error
}

type providerEffect struct {
	ID, Kind, Operation, Currency string
	Amount                        int64
}

func newProviderLedger(t *testing.T, w *world) *providerLedger {
	t.Helper()
	p := &providerLedger{secret: "sk_test_critical", core: stripemock.NewUnstarted(stripemock.Options{Clock: w.clock.Now}), pool: w.pool, schema: "provider_" + strings.ReplaceAll(uuid.NewString(), "-", ""), evidenceRequested: make(chan struct{}, 1)}
	schema := pgx.Identifier{p.schema}.Sanitize()
	_, err := p.pool.Exec(t.Context(), "CREATE SCHEMA "+schema+"; CREATE TABLE "+schema+".effects (id text PRIMARY KEY, kind text NOT NULL, operation text NOT NULL, currency text NOT NULL, amount bigint NOT NULL, object jsonb NOT NULL)")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := p.pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	})
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	t.Cleanup(func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		require.Empty(t, p.errors, "provider economic journal must never silently fail")
	})
	return p
}

func (p *providerLedger) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+p.secret {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"api_key_expired","message":"fixture credential refused"}}`))
		return
	}
	p.mu.Lock()
	noEvidence := p.evidenceUnavailable && r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/v1/payment_intents") || strings.HasPrefix(r.URL.Path, "/v1/charges"))
	p.mu.Unlock()
	if noEvidence {
		select {
		case p.evidenceRequested <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","code":"api_error","message":"fixture evidence temporarily unavailable"}}`))
		return
	}
	response := httptest.NewRecorder()
	p.core.ServeHTTP(response, r)
	// Materialize independent external economic evidence, including a successful
	// response whose connection will be deliberately lost below. Duplicate HTTP
	// requests do not themselves create another economic effect.
	journalContext, cancelJournal := context.WithTimeout(context.Background(), 5*time.Second)
	err := p.persist(journalContext, r, response)
	cancelJournal()
	if err != nil {
		p.mu.Lock()
		p.errors = append(p.errors, err)
		p.mu.Unlock()
		http.Error(w, "fixture journal unavailable", http.StatusServiceUnavailable)
		return
	}
	p.mu.Lock()
	gate := p.responseGate
	p.mu.Unlock()
	if gate != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" && response.Code == http.StatusOK {
		gate.once.Do(func() { close(gate.arrived) })
		select {
		case <-gate.release:
		case <-r.Context().Done():
			return
		}
	}
	p.mu.Lock()
	lose := r.Method == http.MethodPost && response.Code == 200 && ((p.lostResponse && r.URL.Path == "/v1/payment_intents") || (p.lostRefundResponse && r.URL.Path == "/v1/refunds"))
	p.mu.Unlock()
	if lose {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = connection.Close()
		}
		return
	}
	for key, values := range response.Header() {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.Code)
	_, _ = w.Write(response.Body.Bytes())
}

func (p *providerLedger) persist(ctx context.Context, request *http.Request, response *httptest.ResponseRecorder) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	insert := "INSERT INTO " + pgx.Identifier{p.schema, "effects"}.Sanitize() + " (id,kind,operation,currency,amount,object) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(id) DO NOTHING"
	for _, entry := range p.core.Ledger("") {
		object := p.core.PaymentIntent(entry.PaymentIntent)
		data, err := json.Marshal(object)
		if err != nil {
			return err
		}
		currency, ok := object["currency"].(string)
		if !ok {
			return fmt.Errorf("provider payment %s has no currency", entry.PaymentIntent)
		}
		if _, err = tx.Exec(ctx, insert, entry.Charge, "charge", entry.PaymentIntent, currency, entry.Amount, data); err != nil {
			return err
		}
	}
	// Record the exact external refund response with JSON integer semantics.
	// Replaying the same response repeats no economic fact; a new refund ID
	// remains a distinct debit that conservation assertions must account for.
	if request.Method == http.MethodPost && request.URL.Path == "/v1/refunds" && response.Code == http.StatusOK {
		var object map[string]any
		decoder := json.NewDecoder(bytes.NewReader(response.Body.Bytes()))
		decoder.UseNumber()
		if err := decoder.Decode(&object); err != nil {
			return err
		}
		if object["status"] == "succeeded" {
			number, ok := object["amount"].(json.Number)
			if !ok {
				return fmt.Errorf("provider refund amount type %T", object["amount"])
			}
			amount, err := number.Int64()
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, insert, object["id"], "refund", object["payment_intent"], object["currency"], amount, response.Body.Bytes()); err != nil {
				return err
			}
		}
	}

	return tx.Commit(ctx)
}

func (p *providerLedger) effects(t *testing.T) []providerEffect {
	t.Helper()
	rows, err := p.pool.Query(t.Context(), "SELECT id,kind,operation,currency,amount FROM "+pgx.Identifier{p.schema, "effects"}.Sanitize()+" ORDER BY id")
	require.NoError(t, err)
	defer rows.Close()
	var effects []providerEffect
	for rows.Next() {
		var effect providerEffect
		require.NoError(t, rows.Scan(&effect.ID, &effect.Kind, &effect.Operation, &effect.Currency, &effect.Amount))
		effects = append(effects, effect)
	}
	require.NoError(t, rows.Err())
	return effects
}

func (p *providerLedger) interruptPaymentResponses() {
	p.mu.Lock()
	p.lostResponse = true
	p.mu.Unlock()
}

func newPaymentWorld(t *testing.T, configure ...func(*world)) (*authorityWorld, *providerLedger) {
	t.Helper()
	var provider *providerLedger
	a := newAuthorityWorld(t, func(w *world) {
		provider = newProviderLedger(t, w)
		target, err := url.Parse(provider.server.URL)
		require.NoError(t, err)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		w.deps.StripeTransport = loopbackWire{target: target, wire: transport}
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		declaration := w.cfg.Merchants["alpha"]
		declaration.PSPs = openrails.PSPs{"stripe": openrails.StripePSP{AccountID: "acct_e2e", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_critical", WebhookSigningSecret: "whsec_critical_fixture", PublishableKey: "pk_test_critical"}.PSPConfig()}
		declaration.CardVault = "stripe"
		w.cfg.Merchants["alpha"] = declaration
		for _, change := range configure {
			change(w)
		}
	})
	return a, provider
}

func (p *providerLedger) restorePaymentResponses() {
	p.mu.Lock()
	p.lostResponse = false
	p.mu.Unlock()
}

func (p *providerLedger) unavailableEvidence(down bool) {
	p.mu.Lock()
	p.evidenceUnavailable = down
	p.mu.Unlock()
}
