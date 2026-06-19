package intents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeProviderAccountResolver maps provider -> current account; missing = exempt.
type fakeProviderAccountResolver map[string]ProviderAccountIdentity

func (f fakeProviderAccountResolver) ResolveProviderAccount(_ context.Context, provider string) (ProviderAccountIdentity, bool) {
	account, ok := f[provider]
	return account, ok
}

func boundAccount(id uuid.UUID, providerType, accountID string) gen.OpenrailsProviderAccount {
	return gen.OpenrailsProviderAccount{ID: id, ProviderType: providerType, AccountID: accountID}
}

func currentAccount(providerType, accountID string) ProviderAccountIdentity {
	return ProviderAccountIdentity{ProviderKey: "mobius", ProviderType: providerType, AccountID: accountID}
}

// TestRunnerAccountGuardExecute drives the #518 gate through the executor:
// a bound intent only executes when the current credentials resolve to the
// SAME account; a mismatch parks without touching the handler.
func TestRunnerAccountGuardExecute(t *testing.T) {
	boundID := uuid.New()
	cases := []struct {
		name              string
		providerAccountID *uuid.UUID
		current           ProviderAccountResolver
		wantExecuted      bool
	}{
		{"mismatch parks", &boundID, fakeProviderAccountResolver{"mobius": currentAccount("nmi", "new")}, false},
		{"match executes", &boundID, fakeProviderAccountResolver{"mobius": currentAccount("nmi", "old")}, true},
		{"unbound legacy intent executes", nil, fakeProviderAccountResolver{"mobius": currentAccount("nmi", "new")}, true},
		{"unresolvable current executes", &boundID, fakeProviderAccountResolver{}, true},
		{"nil resolver executes", &boundID, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			ledger.accounts[boundID] = boundAccount(boundID, "nmi", "old")
			intent := testIntent("t", OriginUser, 1)
			intent.ProviderAccountID = tc.providerAccountID
			ledger.due = append(ledger.due, intent)

			handler := &fakeHandler{typ: "t", relevance: Relevance{Applicable: true}, execute: Succeeded(nil)}
			r := &Runner{Store: ledger, Registry: NewRegistry(handler), ProviderAccounts: tc.current}
			stats, err := r.RunExecuteOnce(context.Background())
			require.NoError(t, err)

			if tc.wantExecuted {
				assert.Equal(t, 1, handler.executed)
				assert.Equal(t, 1, stats.Succeeded)
			} else {
				assert.Zero(t, handler.executed, "mismatched account must never reach the handler")
				assert.Equal(t, 1, stats.Parked)
				assert.Equal(t, StatusPending, ledger.transition[intent.ID])
				assert.Contains(t, ledger.reasons[intent.ID], "provider account changed since enqueue")
			}
		})
	}
}

// TestRunnerAccountGuardVerify: verification against the wrong account is as
// wrong as executing (NMI "absent = success" would falsely resolve the
// intent) — a mismatch defers the verify, never calls the handler.
func TestRunnerAccountGuardVerify(t *testing.T) {
	ledger := newFakeLedger()
	boundID := uuid.New()
	ledger.accounts[boundID] = boundAccount(boundID, "nmi", "old")
	intent := testIntent("t", OriginSystem, 1)
	intent.Status = StatusUnknownNeedsVerify
	intent.ProviderAccountID = &boundID
	ledger.dueVerify = append(ledger.dueVerify, intent)

	handler := &fakeHandler{typ: "t", relevance: Relevance{Applicable: true}, verify: Succeeded(nil)}
	r := &Runner{Store: ledger, Registry: NewRegistry(handler), ProviderAccounts: fakeProviderAccountResolver{"mobius": currentAccount("nmi", "new")}}
	stats, err := r.RunVerifyOnce(context.Background())
	require.NoError(t, err)

	assert.Zero(t, handler.verified, "mismatched account must never reach Verify")
	assert.Equal(t, 1, stats.Unknown)
	assert.Equal(t, StatusUnknownNeedsVerify, ledger.transition[intent.ID])
	assert.Contains(t, ledger.reasons[intent.ID], "provider account changed since enqueue")
}

const nmiProfileXML = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response><protected>false</protected><is_gateway>true</is_gateway><merchant><company>Acme TEST</company><email>billing@acme.test</email><phone>555-0100</phone></merchant><gateway><company>MobiusPay</company></gateway></nm_response>`

// newNMIProfileServer serves report_type=profile queries, counting calls.
func newNMIProfileServer(t *testing.T, calls *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		require.NoError(t, req.ParseForm())
		require.Equal(t, "profile", req.Form.Get("report_type"))
		if req.Form.Get("security_key") == "rejected" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Specified API key not found</error_response></nm_response>`))
			return
		}
		_, _ = w.Write([]byte(nmiProfileXML))
	}))
}

// TestNMIAccountIdentity: the account identity is the MERCHANT identity from
// the gateway's profile report — derived from the account, not the credential.
func TestNMIAccountIdentity(t *testing.T) {
	var calls atomic.Int64
	srv := newNMIProfileServer(t, &calls)
	defer srv.Close()

	a := &nmi.NMIClient{SecurityKey: "key-one", QueryURL: srv.URL}
	identity, err := a.AccountIdentity()
	require.NoError(t, err)
	assert.Equal(t, "nmi:Acme TEST <billing@acme.test>", identity)

	// Same account behind a rotated key -> same identity.
	b := &nmi.NMIClient{SecurityKey: "key-two", QueryURL: srv.URL}
	identityB, err := b.AccountIdentity()
	require.NoError(t, err)
	assert.Equal(t, identity, identityB)

	// Gateway rejection (bad key) is an error, never a fabricated identity.
	c := &nmi.NMIClient{SecurityKey: "rejected", QueryURL: srv.URL}
	_, err = c.AccountIdentity()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
}

// TestRuntimeProviderAccountsStripe: the stripe account identity is the account id
// from GET /v1/account, cached per secret key — key rotation within the same
// account refetches and matches; a fetch failure resolves to ok=false.
func TestRuntimeProviderAccountsStripe(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		require.Equal(t, "/v1/account", req.URL.Path)
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer sk_test_") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"acct_123"}`))
	}))
	defer srv.Close()

	processors := config.ProcessorSet{
		"stripe": {Type: "stripe", SecretKey: "sk_test_aaa"},
	}
	resolver := NewRuntimeProviderAccounts(&config.Config{}, processors, nil)
	resolver.StripeBaseURL = srv.URL

	ctx := context.Background()
	account, ok := resolver.ResolveProviderAccount(ctx, "stripe")
	require.True(t, ok)
	assert.Equal(t, "stripe", account.ProviderType)
	assert.Equal(t, "acct_123", account.AccountID)

	// Cached: second lookup with the same key makes no HTTP call.
	_, _ = resolver.ResolveProviderAccount(ctx, "stripe")
	assert.Equal(t, int64(1), calls.Load())

	// Same-account key rotation: refetch, same account id, no false park.
	processors["stripe"].SecretKey = "sk_test_bbb"
	account2, ok := resolver.ResolveProviderAccount(ctx, "stripe")
	require.True(t, ok)
	assert.Equal(t, account.AccountID, account2.AccountID)
	assert.Equal(t, int64(2), calls.Load())

	// Fetch failure -> ok=false (guard skipped), not an error.
	processors["stripe"].SecretKey = "rk_live_denied"
	_, ok = resolver.ResolveProviderAccount(ctx, "stripe")
	assert.False(t, ok)

	// Unknown / exempt providers resolve to ok=false.
	_, ok = resolver.ResolveProviderAccount(ctx, "unknown")
	assert.False(t, ok)
}

// TestRuntimeProviderAccountsNMIProvider: NMI-backed provider names resolve
// through the client map; the identity is fetched lazily, cached per
// security key, refetched after key rotation (same account -> same
// account id, no false park), and unresolvable on fetch failure.
func TestRuntimeProviderAccountsNMIProvider(t *testing.T) {
	var calls atomic.Int64
	srv := newNMIProfileServer(t, &calls)
	defer srv.Close()
	ctx := context.Background()

	client := &nmi.NMIClient{SecurityKey: "sec-one", QueryURL: srv.URL}
	resolver := NewRuntimeProviderAccounts(nil, nil, map[string]*nmi.NMIClient{"mobius": client})

	account, ok := resolver.ResolveProviderAccount(ctx, "Mobius")
	require.True(t, ok)
	assert.Equal(t, "nmi", account.ProviderType)
	assert.Equal(t, "Acme TEST <billing@acme.test>", account.AccountID)

	// Cached: no second profile query for the same key.
	_, _ = resolver.ResolveProviderAccount(ctx, "mobius")
	assert.Equal(t, int64(1), calls.Load())

	// Key rotation within the same account: refetch, identical account id.
	client.SecurityKey = "sec-two"
	account2, ok := resolver.ResolveProviderAccount(ctx, "mobius")
	require.True(t, ok)
	assert.Equal(t, account.AccountID, account2.AccountID)
	assert.Equal(t, int64(2), calls.Load())

	// Fetch failure (gateway rejects the key) -> ok=false, guard skipped.
	client.SecurityKey = "rejected"
	_, ok = resolver.ResolveProviderAccount(ctx, "mobius")
	assert.False(t, ok)
}
