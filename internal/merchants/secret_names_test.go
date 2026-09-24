package merchants

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestPSPSecretNameIsCanonicalAndRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		rail, env, account, key string
		want                    string
	}{
		{" Stripe ", "production", "acct_1", "SECRET_KEY", "psps/stripe/live/acct_1/secret_key"},
		{"nmi", "sandbox", " 945280-0000 ", "security_key", "psps/nmi/test/945280-0000/security_key"},
		{"solana", "devnet", "AKnL4", "private_key", "psps/solana/test/AKnL4/private_key"},
		// Account ids are path-escaped so they can never add a path segment.
		{"stripe", "live", "../evil/x", "secret_key", "psps/stripe/live/..%2Fevil%2Fx/secret_key"},
	} {
		name, err := PSPSecretName(tc.rail, tc.env, tc.account, tc.key)
		require.NoError(t, err)
		require.Equal(t, tc.want, name)
		rail, env, account, key, ok, err := ParsePSPSecretName(name)
		require.NoError(t, err)
		require.True(t, ok)
		again, err := PSPSecretName(rail, env, account, key)
		require.NoError(t, err)
		require.Equal(t, name, again)
	}
	for _, bad := range [][4]string{
		{"", "live", "a", "secret_key"},
		{"stripe", "", "a", "secret_key"}, // environment is never defaulted
		{"stripe", "staging", "a", "secret_key"},
		{"stripe", "live", " ", "secret_key"},
		{"stripe", "live", "a", "publishable_key"},
		{"nmi", "live", "mobius", "tokenization_key"}, // public setting, not a secret
		{"stripe", "live", "a", "security_key"},       // another rail's slot
	} {
		_, err := PSPSecretName(bad[0], bad[1], bad[2], bad[3])
		require.Error(t, err, "%q", bad)
	}
}

// SEC-24 item 6: the cleaned name is joined into a Vault path, so traversal
// segments are refused even though callers allowlist names today.
func TestSecretNameRejectsTraversal(t *testing.T) {
	mid := merchant.ID(uuid.New())
	for _, name := range []string{"..", "../../root", "psps/../../other-merchant/nmi/security_key", "stripe/../../..", "./stripe/secret_key", "stripe/./secret_key", "/../", " / "} {
		require.Empty(t, cleanSecretName(name), name)
		require.Error(t, validateSecretRef(mid, name), name)
	}
	for _, name := range []string{"psps/stripe/live/acct_884_test/secret_key", "/psps/nmi/production/100001/security_key/"} {
		require.NotEmpty(t, cleanSecretName(name), name)
		require.NoError(t, validateSecretRef(mid, name), name)
	}
	require.Error(t, validateSecretRef(merchant.ID{}, stripeKeyName))
}

func TestSecretWritability(t *testing.T) {
	solana, err := PSPSecretName("solana", "live", "authority", "private_key")
	require.NoError(t, err)
	for name, writable := range map[string]bool{
		stripeKeyName:                                  true,
		"psps/nmi/test/gw/security_key":                true,
		"psps/stripe/live/acct/webhook_signing_secret": true,
		solana:                          false, // platform-owned signer
		"psps/stripe/live/acct/unknown": false,
		"custodians/basis_theory/test/t1/api_key": true,
		"custodians/nope/test/t1/api_key":         false,
		// #884: retired flat names are not a second spelling.
		"stripe/secret_key":             false,
		"stripe/webhook_signing_secret": false,
		"nmi/mobius/security_key":       false,
		"unknown/provider_key":          false,
	} {
		require.Equal(t, writable, SecretWritable(name), name)
	}
}

func TestCredentialWritesRequirePublication(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	store := NewMemorySecretStore()
	svc, err := NewSecretManagementService(store)
	require.NoError(t, err)
	_, err = NewSecretManagementService(nil)
	require.Error(t, err)

	for _, name := range []string{stripeKeyName, "stripe/secret_key", "nmi/mobius/security_key", "unknown/provider_key"} {
		_, err := svc.PutCredential(ctx, id, name, "sk_test_123")
		require.Error(t, err, name)
	}
	require.Error(t, svc.DeleteCredential(ctx, id, "stripe/secret_key"), "retired names are not deletable")

	// Managed status is write-only: listing exposes metadata, never values.
	_, err = store.Put(ctx, id, "psps/stripe/live/acct_884_test/webhook_signing_secret", "whsec_123")
	require.NoError(t, err)
	statuses, err := svc.ListSecretStatuses(ctx, id)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	require.True(t, statuses[0].Configured)
	require.Equal(t, 1, statuses[0].Version)
	require.NotContains(t, fmt.Sprintf("%+v", statuses), "whsec_123")
	require.NoError(t, svc.DeleteCredential(ctx, id, "psps/stripe/live/acct_884_test/webhook_signing_secret"))
	_, err = store.Get(ctx, id, "psps/stripe/live/acct_884_test/webhook_signing_secret")
	require.ErrorIs(t, err, ErrSecretNotFound)
}

func TestValidateCredentialNeverPersists(t *testing.T) {
	ctx, id := t.Context(), merchant.ID(uuid.New())
	store := NewMemorySecretStore()
	svc := &Service{secrets: store}
	probeErr := errors.New("Stripe refused access")
	for _, tc := range []struct {
		value   string
		probe   error
		probed  bool
		wantErr error
	}{
		{value: "sk_test_123", probed: true},
		{value: "sk_live_123", probed: true},
		{value: "rk_test_123", probed: true},
		{value: "rk_live_123", probe: probeErr, probed: true, wantErr: probeErr},
		{value: "pk_test_123"},
		{value: "whsec_123"},
		{value: "not-stripe"},
		{value: "   "}, // empty loads the stored value, which is absent
	} {
		probed := false
		err := svc.ValidateCredential(ctx, id, stripeKeyName, tc.value, func(_ context.Context, supplied string) error {
			probed = true
			require.Equal(t, tc.value, supplied)
			return tc.probe
		})
		require.Equal(t, tc.probed, probed, tc.value)
		switch {
		case tc.wantErr != nil:
			require.ErrorIs(t, err, tc.wantErr)
		case tc.probed:
			require.NoError(t, err)
		default:
			require.Error(t, err, tc.value)
		}
	}
	names, err := store.List(ctx, id)
	require.NoError(t, err)
	require.Empty(t, names, "validation never saves")
}

// #662: the PSP id is a pure function of the canonical global natural key.
func TestPSPIDDerivesFromCanonicalNaturalKey(t *testing.T) {
	base := PspID("nmi", "live", "945280-0000")
	for _, alias := range [][3]string{{"NMI", "live", "945280-0000"}, {" nmi ", "LIVE", "945280-0000"}, {"nmi", "production", "945280-0000"}, {"nmi", "mainnet", "945280-0000"}, {"nmi", "live", " 945280-0000 "}} {
		require.Equal(t, base, PspID(alias[0], alias[1], alias[2]), "%q", alias)
	}
	for _, other := range [][3]string{{"stripe", "live", "945280-0000"}, {"nmi", "test", "945280-0000"}, {"nmi", "live", "945280-0001"}} {
		require.NotEqual(t, base, PspID(other[0], other[1], other[2]), "%q", other)
	}
	id, rail, env, account := PSPNaturalKey("NMI", "production", " 945280-0000 ")
	require.Equal(t, []any{base, "nmi", "live", "945280-0000"}, []any{id, rail, env, account})
}
