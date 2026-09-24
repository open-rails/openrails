package railresolve

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type exactSecrets struct {
	secret merchants.Secret
	err    error
	owner  merchant.ID
	name   string
	floor  int
}

func (s *exactSecrets) Get(_ context.Context, owner merchant.ID, name string) (merchants.Secret, error) {
	s.owner, s.name = owner, name
	return s.secret, s.err
}

func (s *exactSecrets) GetVersion(ctx context.Context, owner merchant.ID, name string, floor int) (merchants.Secret, error) {
	s.floor = floor
	return s.Get(ctx, owner, name)
}

// or#812: the custodian row's rotation floor is exact; owner, kind and posture
// environment must all match before any credential is read.
func TestHyperSwitchCredentialResolution(t *testing.T) {
	owner := merchant.ID(uuid.New())
	base := gen.OpenrailsCustodian{ID: uuid.New(), MerchantID: owner.UUID(), Kind: "hyperswitch", Environment: "test", AccountID: "merchant_A",
		Settings: []byte(`{"public_api_key":"public_A","profile_id":"profile_A"}`), CredentialVersions: []byte(`{"api_key":2}`)}
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: "https://owned-custody.example"}}
	versions := func(v string) func(*gen.OpenrailsCustodian) {
		return func(r *gen.OpenrailsCustodian) { r.CredentialVersions = []byte(v) }
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*gen.OpenrailsCustodian)
		version int
		secret  string
		err     error
		ok      bool
		floor   int
	}{
		{name: "current key", version: 2, secret: "k", ok: true, floor: 2},
		{name: "archived still addressable", mutate: func(r *gen.OpenrailsCustodian) { r.Archived = true }, version: 2, secret: "k", ok: true, floor: 2},
		{name: "unrotated declaration", mutate: versions(`{}`), version: 1, secret: "k", ok: true},
		{name: "missing versions document", mutate: versions(""), version: 2, secret: "k"},
		{name: "null versions document", mutate: versions(`null`), version: 2, secret: "k"},
		{name: "string floor", mutate: versions(`{"api_key":"2"}`), version: 2, secret: "k"},
		{name: "null floor", mutate: versions(`{"api_key":null}`), version: 2, secret: "k"},
		{name: "negative floor", mutate: versions(`{"api_key":-1}`), version: 2, secret: "k"},
		{name: "null settings", mutate: func(r *gen.OpenrailsCustodian) { r.Settings = []byte(`null`) }, version: 2, secret: "k"},
		{name: "stale key", version: 1, secret: "k", floor: 2},
		{name: "newer unpublished key", version: 3, secret: "k", floor: 2},
		{name: "empty key", version: 2, secret: " ", floor: 2},
		{name: "missing secret", err: merchants.ErrSecretNotFound, floor: 2},
		{name: "foreign owner", mutate: func(r *gen.OpenrailsCustodian) { r.MerchantID = uuid.New() }, version: 2, secret: "k"},
		{name: "live row under sandbox", mutate: func(r *gen.OpenrailsCustodian) { r.Environment = "live" }, version: 2, secret: "k"},
		{name: "wrong kind", mutate: func(r *gen.OpenrailsCustodian) { r.Kind = "basis_theory" }, version: 2, secret: "k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := base
			if tc.mutate != nil {
				tc.mutate(&row)
			}
			secrets := &exactSecrets{secret: merchants.Secret{Value: tc.secret, Version: tc.version}, err: tc.err}
			client, err := HyperSwitchClient(t.Context(), cfg, secrets, owner, row)
			require.Equal(t, tc.ok, err == nil, "err=%v", err)
			require.Equal(t, tc.ok, client != nil)
			if tc.ok {
				require.Equal(t, owner, secrets.owner)
				require.Equal(t, "custodians/hyperswitch/test/merchant_A/api_key", secrets.name)
			}
			require.Equal(t, tc.floor, secrets.floor)
		})
	}
	_, err := HyperSwitchClient(t.Context(), cfg, nil, owner, base)
	require.ErrorIs(t, err, hyperswitch.ErrBinding)
}

// or#881: the declared tokens map IS the accepted set; built-in symbols take the
// registry mint (restating it fails closed); mainnet pricing policy only subtracts.
func TestSolanaRailConfigFromSettings(t *testing.T) {
	const customMint = "MyTk1111111111111111111111111111111111111111"
	declared := config.SolanaAccountSettings{Tokens: map[string]config.TokenConfig{"USDC": {Name: "Dollars"}, "MYTK": {Name: "My Token", Mint: customMint}}}

	out, err := SolanaRailConfigFromSettings(declared, true)
	require.NoError(t, err)
	require.Equal(t, "devnet", out.Network)
	require.Len(t, out.Tokens, 2, "undeclared registry tokens (SOL, PYUSD) must not be accepted")
	require.Equal(t, solanatokens.ForNetwork("devnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Equal(t, "Dollars", out.Tokens["USDC"].Name)
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)

	out, err = SolanaRailConfigFromSettings(declared, false)
	require.NoError(t, err)
	require.Equal(t, solanatokens.ForNetwork("mainnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Len(t, out.Tokens, 1, "feedless non-stablecoin is disabled on mainnet")

	_, err = SolanaRailConfigFromSettings(config.SolanaAccountSettings{Tokens: map[string]config.TokenConfig{"USDC": {Mint: "Ovr11111111111111111111111111111111111111111"}}}, true)
	require.ErrorContains(t, err, "USDC is a built-in token")

	for testMode, network := range map[bool]string{true: "devnet", false: "mainnet"} {
		out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, testMode)
		require.NoError(t, err)
		require.Equal(t, map[string]config.TokenConfig{"USDC": solanatokens.ForNetwork(network)["USDC"]}, out.Tokens, "%s default is USDC alone", network)
	}
}
