package config

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestMerchantBindingIsHostOnlyAndCopiedAtDeclaration(t *testing.T) {
	binding := &billing.MerchantBinding{ID: billing.MerchantID(uuid.New()), TenantID: uuid.NewString()}
	cfg := &Config{Merchants: map[string]MerchantDeclaration{"host-one": {Binding: binding}}}
	declarations, refused, err := DeclaredMerchants(cfg)
	require.NoError(t, err)
	require.Empty(t, refused)
	require.Len(t, declarations, 1)
	want := *binding
	binding.TenantID = uuid.NewString()
	require.Equal(t, want, *declarations[0].Binding)
	encoded, err := json.Marshal(declarations[0])
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "TenantID")
	require.NotContains(t, string(encoded), "Binding")
	_, err = ParseMerchantDeclaration([]byte("slug: host-one\nbinding: {id: example, tenant_id: example}\n"))
	require.Error(t, err, "merchant documents must not claim a host's binding")
}
