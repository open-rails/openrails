package merchants

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPSPSecretNameIsCanonicalAndRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		rail, env, account, key string
		want                    string
	}{
		{" Stripe ", "production", "acct_1", "SECRET_KEY", "psps/stripe/live/acct_1/secret_key"},
		{"nmi", "sandbox", " 999999-0000 ", "security_key", "psps/nmi/test/999999-0000/security_key"},
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

// SEC-24 item 6: traversal segments never name a credential.
func TestSecretNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"..", "../../root", "psps/../../other-merchant/nmi/security_key", "stripe/../../..", "./stripe/secret_key", "stripe/./secret_key", "/../", " / "} {
		require.Empty(t, cleanSecretName(name), name)
	}
	for _, name := range []string{"psps/stripe/live/acct_884_test/secret_key", "/psps/nmi/production/100001/security_key/"} {
		require.NotEmpty(t, cleanSecretName(name), name)
	}
}

const stripeKeyName = "psps/stripe/live/acct_884_test/secret_key"

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

// #662: the PSP id is a pure function of the canonical global natural key.
func TestPSPIDDerivesFromCanonicalNaturalKey(t *testing.T) {
	base := PspID("nmi", "live", "999999-0000")
	for _, alias := range [][3]string{{"NMI", "live", "999999-0000"}, {" nmi ", "LIVE", "999999-0000"}, {"nmi", "production", "999999-0000"}, {"nmi", "mainnet", "999999-0000"}, {"nmi", "live", " 999999-0000 "}} {
		require.Equal(t, base, PspID(alias[0], alias[1], alias[2]), "%q", alias)
	}
	for _, other := range [][3]string{{"stripe", "live", "999999-0000"}, {"nmi", "test", "999999-0000"}, {"nmi", "live", "999999-0001"}} {
		require.NotEqual(t, base, PspID(other[0], other[1], other[2]), "%q", other)
	}
	id, rail, env, account := PSPNaturalKey("NMI", "production", " 999999-0000 ")
	require.Equal(t, []any{base, "nmi", "live", "999999-0000"}, []any{id, rail, env, account})
}
