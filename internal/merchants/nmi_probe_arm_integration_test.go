//go:build integration

package merchants

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
)

// nmiProbeArmTestServer fakes the NMI v5 gateway's test-mode-probe surface
// (POST /payments/auth + POST /payments/:id/void). authCode is the v5
// "response" value: "1" approved (simulating), "2" declined (LIVE account),
// "3" a gateway-level error (indeterminate).
func nmiProbeArmTestServer(t *testing.T, authCode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/":
			require.NoError(t, r.ParseForm())
			require.NotEmpty(t, r.Form.Get("security_key"))
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/auth":
			var req struct {
				PaymentDetails struct {
					CardNumber string `json:"card_number"`
				} `json:"payment_details"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, "4111111111111111", req.PaymentDetails.CardNumber)
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"` + authCode + `","response_text":"PROBE","response_code":"100"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/probe-txn/void":
			_, _ = w.Write([]byte(`{"object":"transaction","id":"probe-txn","response":"1","response_text":"SUCCESS"}`))
		default:
			t.Errorf("unexpected NMI probe request %s %s", r.Method, r.URL.Path)
		}
	}))
}

func countNMIAccounts(t *testing.T, svc *Service, accountID string) int {
	t.Helper()
	var count int
	require.NoError(t, svc.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM billing.psps WHERE rail = 'nmi' AND account_id = $1
	`, accountID).Scan(&count))
	return count
}

// TestUpsertPaymentProviderConfigRefusesLiveNMIUnderTestMode reinstates #348
// at the MODE-2 (API) arm boundary: under test_mode, arming an NMI account
// whose credentials belong to a LIVE gateway must be refused, and nothing
// may be persisted (no psps row, no stored secret).
func TestUpsertPaymentProviderConfigRefusesLiveNMIUnderTestMode(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), newPublicationTestStore(t, pool), "test")
	require.NoError(t, err)
	server := nmiProbeArmTestServer(t, "2") // declined -> LIVE account
	defer server.Close()
	svc.nmiProbeV5BaseURL = server.URL
	svc.nmiCredentialProbeQueryURL = server.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-live-348", PermissionGroupID: "group-probe-live-348"})
	require.NoError(t, err)

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-live",
		Credentials: map[string]string{"security_key": "live-security-key"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PRODUCTION NMI credentials detected while test_mode is enabled")
	require.Equal(t, 0, countNMIAccounts(t, svc, "arm-348-live"), "a refused arm must never persist the PSP row")

	_, err = svc.GetPaymentProviderConfig(ctx, tn.ID, "nmi", "test")
	require.ErrorIs(t, err, ErrPaymentProviderNotFound, "a refused arm must not create a resolvable provider config")
}

// TestUpsertPaymentProviderConfigArmsSimulatedNMIUnderTestMode is the
// control: a genuinely sandboxed account (approves the probe) arms normally.
func TestUpsertPaymentProviderConfigArmsSimulatedNMIUnderTestMode(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), newPublicationTestStore(t, pool), "test")
	require.NoError(t, err)
	server := nmiProbeArmTestServer(t, "1") // approved -> simulating
	defer server.Close()
	svc.nmiProbeV5BaseURL = server.URL
	svc.nmiCredentialProbeQueryURL = server.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-sim-348", PermissionGroupID: "group-probe-sim-348"})
	require.NoError(t, err)

	cfg, err := svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-sim",
		Credentials: map[string]string{"security_key": "sandbox-security-key"},
	})
	require.NoError(t, err)
	require.Equal(t, "arm-348-sim", cfg.AccountID)
	require.Equal(t, 1, countNMIAccounts(t, svc, "arm-348-sim"))
}

// An inconclusive sandbox probe cannot authorize an account.
func TestUpsertPaymentProviderConfigProbeIndeterminateRefuses(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), newPublicationTestStore(t, pool), "test")
	require.NoError(t, err)
	server := nmiProbeArmTestServer(t, "3") // gateway-level error -> indeterminate
	defer server.Close()
	svc.nmiProbeV5BaseURL = server.URL
	svc.nmiCredentialProbeQueryURL = server.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-indeterminate-348", PermissionGroupID: "group-probe-indeterminate-348"})
	require.NoError(t, err)

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-indeterminate",
		Credentials: map[string]string{"security_key": "unclear-security-key"},
	})
	require.ErrorContains(t, err, "qualification failed")
	require.Zero(t, countNMIAccounts(t, svc, "arm-348-indeterminate"))
}

// Every arm requires fresh qualification, even after a previous conclusive result.
func TestUpsertPaymentProviderConfigRequiresFreshProbe(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), newPublicationTestStore(t, pool), "test")
	require.NoError(t, err)
	server := nmiProbeArmTestServer(t, "2") // declined -> LIVE account
	svc.nmiProbeV5BaseURL = server.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-qualification-348", PermissionGroupID: "group-probe-qualification-348"})
	require.NoError(t, err)

	req := UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-qualification",
		Credentials: map[string]string{"security_key": "live-qualification-key"},
	}
	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "PRODUCTION NMI credentials")

	server.Close() // any re-probe now fails as a transport error (indeterminate)

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", req)
	require.Error(t, err, "a transport failure must refuse the arm")
	require.Contains(t, err.Error(), "qualification failed")
}

// TestUpsertPaymentProviderConfigSkipsTestModeProbeOutsideTestMode proves
// production deployments never run the v5 test-card arm probe. The read-only
// credential-validation query still runs.
func TestUpsertPaymentProviderConfigSkipsTestModeProbeOutsideTestMode(t *testing.T) {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), newPublicationTestStore(t, pool), "live")
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/", r.URL.Path, "production must skip the v5 test-mode probe")
		require.NoError(t, r.ParseForm())
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	defer server.Close()
	svc.nmiProbeV5BaseURL = server.URL
	svc.nmiCredentialProbeQueryURL = server.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-prod-348", PermissionGroupID: "group-probe-prod-348"})
	require.NoError(t, err)

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-prod",
		Credentials: map[string]string{"security_key": "live-prod-key"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, countNMIAccounts(t, svc, "arm-348-prod"))
}

// TestUpsertPaymentProviderConfigRefusesUsingExistingStoredKey proves the
// probe resolves the EFFECTIVE security_key even when this particular arm
// request doesn't supply one (e.g. rotating only public_config) — a
// previously-armed live account can't slip a follow-up update through by
// omitting security_key.
func TestUpsertPaymentProviderConfigRefusesUsingExistingStoredKey(t *testing.T) {
	pool := newTestPool(t)
	store := newPublicationTestStore(t, pool)
	svc, err := NewService(db.WrapPool(pool, ""), store, "test")
	require.NoError(t, err)
	simServer := nmiProbeArmTestServer(t, "1") // first arm: simulating
	svc.nmiProbeV5BaseURL = simServer.URL
	svc.nmiCredentialProbeQueryURL = simServer.URL

	ctx := context.Background()
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "probe-existing-348", PermissionGroupID: "group-probe-existing-348"})
	require.NoError(t, err)

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:   "arm-348-existing",
		Credentials: map[string]string{"security_key": "was-sandbox-then-rotated-live"},
	})
	require.NoError(t, err)
	simServer.Close()

	// Simulate the credential having been rotated to a LIVE key out of band
	// (a merchant editing the raw secret store directly is out of scope for
	// this codebase, but a follow-up arm that forgets to resend
	// security_key must still be checked against what's on file).
	name, err := PSPSecretName("nmi", "test", "arm-348-existing", "security_key")
	require.NoError(t, err)
	_, err = store.Put(ctx, tn.ID, name, "now-a-live-key")
	require.NoError(t, err)

	liveServer := nmiProbeArmTestServer(t, "2") // declined -> LIVE account
	defer liveServer.Close()
	svc.nmiProbeV5BaseURL = liveServer.URL

	_, err = svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0),
		AccountID:    "arm-348-existing",
		PublicConfig: map[string]string{"note": "unrelated update"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PRODUCTION NMI credentials")
}
