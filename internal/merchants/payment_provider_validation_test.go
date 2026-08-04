package merchants

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbePaymentProviderCredentialsNMI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "security-key", r.Form.Get("security_key"))
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	t.Cleanup(server.Close)

	svc := &Service{
		secrets:                    NewMemorySecretStore(),
		nmiCredentialProbeQueryURL: server.URL,
	}
	validated, err := svc.probePaymentProviderCredentials(
		t.Context(), merchant.ID(uuid.New()), "nmi", "test", "gateway", map[string]string{"security_key": "security-key"},
	)
	require.NoError(t, err)
	assert.True(t, validated)
}

func TestProbePaymentProviderCredentialsCCBillUsesEffectivePair(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		assert.Equal(t, "new-user", r.Form.Get("username"))
		assert.Equal(t, "stored-pass", r.Form.Get("password"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	id := merchant.ID(uuid.New())
	store := NewMemorySecretStore()
	passwordName, err := PSPSecretName("ccbill", "live", "900000-0000", "datalink_password")
	require.NoError(t, err)
	_, err = store.Put(t.Context(), id, passwordName, "stored-pass")
	require.NoError(t, err)

	svc := &Service{
		secrets:                      store,
		ccbillCredentialProbeBaseURL: server.URL,
	}
	validated, err := svc.probePaymentProviderCredentials(
		t.Context(), id, "ccbill", "live", "900000-0000", map[string]string{"datalink_username": "new-user"},
	)
	require.NoError(t, err)
	assert.True(t, validated)
}

func TestProbePaymentProviderCredentialsCCBillRequiresCompletePair(t *testing.T) {
	t.Parallel()

	svc := &Service{secrets: NewMemorySecretStore()}
	validated, err := svc.probePaymentProviderCredentials(
		t.Context(), merchant.ID(uuid.New()), "ccbill", "live", "900000-0000", map[string]string{"datalink_username": "user"},
	)
	require.ErrorContains(t, err, "required together")
	assert.False(t, validated)
}
