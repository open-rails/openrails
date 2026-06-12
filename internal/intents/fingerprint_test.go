package intents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// fakeFingerprints maps provider -> current fingerprint; missing = exempt.
type fakeFingerprints map[string]string

func (f fakeFingerprints) AccountFingerprint(_ context.Context, provider string) (string, bool) {
	fp, ok := f[provider]
	return fp, ok
}

func strPtr(s string) *string { return &s }

// TestRunnerAccountGuardExecute drives the #365 gate through the executor:
// a stamped intent only executes when the current credentials resolve to the
// SAME account; a mismatch parks without touching the handler.
func TestRunnerAccountGuardExecute(t *testing.T) {
	cases := []struct {
		name         string
		stamp        *string
		fingerprints FingerprintSource
		wantExecuted bool
	}{
		{"mismatch parks", strPtr("nmi:old"), fakeFingerprints{"mobius": "nmi:new"}, false},
		{"match executes", strPtr("nmi:same"), fakeFingerprints{"mobius": "nmi:same"}, true},
		{"unstamped (pre-#365) executes", nil, fakeFingerprints{"mobius": "nmi:new"}, true},
		{"unresolvable current executes", strPtr("nmi:old"), fakeFingerprints{}, true},
		{"nil source executes", strPtr("nmi:old"), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ledger := newFakeLedger()
			intent := testIntent("t", OriginUser, 1)
			intent.AccountFingerprint = tc.stamp
			ledger.due = append(ledger.due, intent)

			handler := &fakeHandler{typ: "t", relevance: Relevance{Applicable: true}, execute: Succeeded(nil)}
			r := &Runner{Store: ledger, Registry: NewRegistry(handler), Fingerprints: tc.fingerprints}
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
	intent := testIntent("t", OriginSystem, 1)
	intent.Status = StatusUnknownNeedsVerify
	intent.AccountFingerprint = strPtr("nmi:old")
	ledger.dueVerify = append(ledger.dueVerify, intent)

	handler := &fakeHandler{typ: "t", relevance: Relevance{Applicable: true}, verify: Succeeded(nil)}
	r := &Runner{Store: ledger, Registry: NewRegistry(handler), Fingerprints: fakeFingerprints{"mobius": "nmi:new"}}
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

// TestNMIAccountIdentity: the fingerprint is the MERCHANT identity from the
// gateway's profile report — derived from the account, not the credential.
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

// TestRuntimeFingerprintsStripe: the stripe fingerprint is the account id
// from GET /v1/account, cached per secret key — key rotation within the same
// account refetches and matches; a fetch failure resolves to ok=false.
func TestRuntimeFingerprintsStripe(t *testing.T) {
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

	cfg := &config.Config{Processors: map[string]*config.ProcessorConfig{
		"stripe": {Type: "stripe", SecretKey: "sk_test_aaa"},
	}}
	f := NewRuntimeFingerprints(cfg, nil)
	f.StripeBaseURL = srv.URL

	ctx := context.Background()
	fp, ok := f.AccountFingerprint(ctx, "stripe")
	require.True(t, ok)
	assert.Equal(t, "stripe:acct_123", fp)

	// Cached: second lookup with the same key makes no HTTP call.
	_, _ = f.AccountFingerprint(ctx, "stripe")
	assert.Equal(t, int64(1), calls.Load())

	// Same-account key rotation: refetch, same fingerprint, no false park.
	cfg.Processors["stripe"].SecretKey = "sk_test_bbb"
	fp2, ok := f.AccountFingerprint(ctx, "stripe")
	require.True(t, ok)
	assert.Equal(t, fp, fp2)
	assert.Equal(t, int64(2), calls.Load())

	// Fetch failure -> ok=false (guard skipped), not an error.
	cfg.Processors["stripe"].SecretKey = "rk_live_denied"
	_, ok = f.AccountFingerprint(ctx, "stripe")
	assert.False(t, ok)

	// Unknown / exempt providers resolve to ok=false.
	_, ok = f.AccountFingerprint(ctx, "solana")
	assert.False(t, ok)
}

// TestRuntimeFingerprintsNMIProvider: NMI-backed provider names resolve
// through the client map; the identity is fetched lazily, cached per
// security key, refetched after key rotation (same account -> same
// fingerprint, no false park), and unresolvable on fetch failure.
func TestRuntimeFingerprintsNMIProvider(t *testing.T) {
	var calls atomic.Int64
	srv := newNMIProfileServer(t, &calls)
	defer srv.Close()
	ctx := context.Background()

	client := &nmi.NMIClient{SecurityKey: "sec-one", QueryURL: srv.URL}
	f := NewRuntimeFingerprints(nil, map[string]*nmi.NMIClient{"mobius": client})

	fp, ok := f.AccountFingerprint(ctx, "Mobius")
	require.True(t, ok)
	assert.Equal(t, "nmi:Acme TEST <billing@acme.test>", fp)

	// Cached: no second profile query for the same key.
	_, _ = f.AccountFingerprint(ctx, "mobius")
	assert.Equal(t, int64(1), calls.Load())

	// Key rotation within the same account: refetch, identical fingerprint.
	client.SecurityKey = "sec-two"
	fp2, ok := f.AccountFingerprint(ctx, "mobius")
	require.True(t, ok)
	assert.Equal(t, fp, fp2)
	assert.Equal(t, int64(2), calls.Load())

	// Fetch failure (gateway rejects the key) -> ok=false, guard skipped.
	client.SecurityKey = "rejected"
	_, ok = f.AccountFingerprint(ctx, "mobius")
	assert.False(t, ok)
}
