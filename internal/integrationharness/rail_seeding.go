//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/pkg/merchant"
)

// SeedRailMerchantAccounts is the test-side Layer-A writer (#788): it converges
// the given rail credential set into the SAME armed state production writers
// (MODE 1 manifest convergence / MODE 2 API writes) produce —
// psps rows + scoped secrets in the runtime's merchant
// secret store. Everything downstream (checkout, webhooks, pulls, rebills)
// then resolves it through the ONE Layer-C seam exactly like production.
func SeedRailMerchantAccounts(ctx context.Context, t *testing.T, rt *app.Runtime, mid merchant.ID, set config.PSPSet) {
	t.Helper()
	if len(set) == 0 {
		return
	}
	require.NotNil(t, rt, "runtime required")
	require.NotNil(t, rt.Merchants, "merchants service must be armed before seeding rail accounts")
	store := rt.Merchants.Secrets()
	require.NotNil(t, store, "merchant secret store must be armed before seeding rail accounts")
	testMode := rt.Config != nil && rt.Config.IsTestMode()

	for name, proc := range set {
		if proc == nil {
			continue
		}
		pspKey := strings.ToLower(strings.TrimSpace(name))
		require.NotEmpty(t, pspKey, "PSP key required")
		rail := string(proc.EffectiveRail(name))
		require.NotEmpty(t, rail, "rail for account %q", name)
		accountID := proc.EffectiveAccountID()
		require.NotEmpty(t, accountID, "account_id for account %q", name)
		environment := config.ExpectedProviderEnvironment(testMode)

		var evidence []byte
		if settings := railAccountSettings(proc, testMode); len(settings) > 0 {
			raw, err := json.Marshal(map[string]any{"settings": settings})
			require.NoError(t, err)
			evidence = raw
		}

		id, nRail, nEnv, nAccount := merchants.PSPNaturalKey(rail, environment, accountID)
		mctx := merchant.WithID(ctx, mid)
		archived := proc.Archived
		require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
			// Key is the declared manifest key, not decoration: the #848 wire
			// selector names a PSP by it, so a NULL key makes a merchant's
			// second armed PSP unreachable (or#890).
			_, err := rt.DB.Gen(cctx).UpsertPSP(cctx, gen.UpsertPSPParams{
				ID:          id,
				MerchantID:  mid.UUID(),
				Rail:        nRail,
				Environment: &nEnv,
				AccountID:   nAccount,
				Key:         &pspKey,
				Archived:    &archived,
				Evidence:    evidence,
			})
			return err
		}), "seed rail account %s/%s", rail, accountID)

		for key, value := range railAccountSecrets(proc) {
			if strings.TrimSpace(value) == "" {
				continue
			}
			secretName, err := merchants.PSPSecretName(nRail, nEnv, nAccount, key)
			require.NoError(t, err)
			_, err = store.Put(ctx, mid, secretName, value)
			require.NoError(t, err, "seed secret %s", secretName)
		}
	}
}

// railAccountSecrets maps a typed rail credential block onto the canonical
// per-rail secret keys (#711 — same names the manifest/API writers store).
func railAccountSecrets(proc *config.PSPConfig) map[string]string {
	out := map[string]string{}
	switch {
	case proc.Stripe != nil:
		out["secret_key"] = proc.Stripe.SecretKey
		out["webhook_signing_secret"] = proc.Stripe.WebhookSigningSecret
		out["webhook_signing_secret_thin"] = proc.Stripe.WebhookSigningSecretThin
	case proc.CCBill != nil:
		out["salt"] = proc.CCBill.Salt
		out["datalink_username"] = proc.CCBill.DataLinkUsername
		out["datalink_password"] = proc.CCBill.DataLinkPassword
	case proc.NMI != nil:
		out["security_key"] = proc.NMI.SecurityKey
		out["webhook_signing_secret"] = proc.NMI.WebhookSigningSecret
	}
	return out
}

// railAccountSettings maps the typed Solana block onto the rail-account
// settings evidence shape (#711).
func railAccountSettings(proc *config.PSPConfig, testMode bool) map[string]any {
	if proc.Solana == nil {
		return nil
	}
	network := "mainnet"
	if testMode {
		network = "devnet"
	}
	registry := solanatokens.ForNetwork(network)
	settings := map[string]any{}
	if proc.Solana.RPCProvider != "" {
		settings[config.SolanaSettingRPCProvider] = proc.Solana.RPCProvider
	}
	if proc.Solana.RPCAPIKey != "" {
		settings[config.SolanaSettingRPCAPIKey] = proc.Solana.RPCAPIKey
	}
	if len(proc.Solana.Tokens) > 0 {
		tokens := map[string]any{}
		for symbol, token := range proc.Solana.Tokens {
			// or#881: a built-in symbol is SELECTED — its mint comes from the
			// registry and restating it is a push error. Emit the declaration a
			// real manifest would carry, not a restatement.
			entry := map[string]any{}
			if token.Name != "" {
				entry["name"] = token.Name
			}
			if _, builtin := registry[strings.ToUpper(strings.TrimSpace(symbol))]; !builtin {
				entry["mint"] = token.Mint
			}
			tokens[symbol] = entry
		}
		settings[config.SolanaSettingTokens] = tokens
	}
	// #592: the declared account_id IS the merchant wallet; the checkout money
	// path reads it as the recipient_wallet setting.
	if wallet := strings.TrimSpace(proc.AccountID); wallet != "" {
		settings[config.SolanaSettingRecipientWallet] = wallet
	}
	return settings
}
