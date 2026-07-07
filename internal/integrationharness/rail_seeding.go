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
	"github.com/open-rails/openrails/pkg/merchant"
)

// SeedRailMerchantAccounts is the test-side Layer-A writer (#788): it converges
// the given rail credential set into the SAME armed state production writers
// (MODE 1 manifest convergence / MODE 2 API writes) produce —
// rail_merchant_accounts rows + scoped secrets in the runtime's merchant
// secret store. Everything downstream (checkout, webhooks, pulls, rebills)
// then resolves it through the ONE Layer-C seam exactly like production.
func SeedRailMerchantAccounts(ctx context.Context, t *testing.T, rt *app.Runtime, mid merchant.ID, set config.RailMerchantAccountSet) {
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
		rail := string(proc.EffectiveRail(name))
		require.NotEmpty(t, rail, "rail for account %q", name)
		accountID := proc.EffectiveAccountID()
		require.NotEmpty(t, accountID, "account_id for account %q", name)
		environment := proc.EffectiveEnvironment(testMode)

		var evidence []byte
		if settings := railAccountSettings(proc); len(settings) > 0 {
			raw, err := json.Marshal(map[string]any{"settings": settings})
			require.NoError(t, err)
			evidence = raw
		}

		id, nRail, nEnv, nAccount := merchants.RailMerchantAccountNaturalKey(rail, environment, accountID)
		mctx := merchant.WithID(ctx, mid)
		archived := proc.Archived
		require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
			_, err := rt.DB.Gen(cctx).UpsertRailMerchantAccount(cctx, gen.UpsertRailMerchantAccountParams{
				ID:          id,
				MerchantID:  mid.UUID(),
				Rail:        nRail,
				Environment: &nEnv,
				AccountID:   nAccount,
				Archived:    &archived,
				Evidence:    evidence,
			})
			return err
		}), "seed rail account %s/%s", rail, accountID)

		for key, value := range railAccountSecrets(proc) {
			if strings.TrimSpace(value) == "" {
				continue
			}
			secretName, err := merchants.RailMerchantAccountSecretName(nRail, nEnv, nAccount, key)
			require.NoError(t, err)
			_, err = store.Put(ctx, mid, secretName, value)
			require.NoError(t, err, "seed secret %s", secretName)
		}
	}
}

// railAccountSecrets maps a typed rail credential block onto the canonical
// per-rail secret keys (#711 — same names the manifest/API writers store).
func railAccountSecrets(proc *config.RailMerchantAccountConfig) map[string]string {
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
func railAccountSettings(proc *config.RailMerchantAccountConfig) map[string]any {
	if proc.Solana == nil {
		return nil
	}
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
			tokens[symbol] = map[string]any{"mint": token.Mint, "name": token.Name, "decimals": token.Decimals}
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
