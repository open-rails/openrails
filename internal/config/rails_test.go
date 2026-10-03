package config

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func postureConfig(sandbox bool) *Config {
	posture := CredentialPostureLive
	if sandbox {
		posture = CredentialPostureSandbox
	}
	return &Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: posture}
}

func stripeRail(key string) *PSPConfig {
	return &PSPConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: key, WebhookSigningSecret: "whsec_fixture"}}
}

// A key never runs under the opposite posture, archived accounts included, and
// refusal never rewrites the credential.
func TestStripeCredentialsMatchDeclaredPosture(t *testing.T) {
	for _, prefix := range []string{"sk", "rk"} {
		for _, keyMode := range []string{"test", "live"} {
			for _, sandbox := range []bool{true, false} {
				key := prefix + "_" + keyMode + "_abc123"
				for _, archived := range []bool{false, true} {
					rails := PSPSet{"stripe": stripeRail(key)}
					rails["stripe"].Archived = archived
					err := ValidateRailSet(postureConfig(sandbox), rails)
					require.Equal(t, sandbox == (keyMode == "test"), err == nil, "%s sandbox=%v archived=%v", key, sandbox, archived)
					require.Equal(t, key, rails["stripe"].Stripe.SecretKey)
				}
			}
		}
	}
	for _, malformed := range []string{"sk_test_", "pk_test_abc", "whsec_abc"} {
		require.ErrorContains(t, ValidateStripeCredentialPosture(postureConfig(true), malformed), "invalid Stripe secret key format")
	}
	require.NoError(t, ValidateStripeCredentialPosture(postureConfig(false), ""), "an absent key only disables checkout")

	for _, sandbox := range []bool{true, false} {
		cfg := postureConfig(sandbox)
		require.Equal(t, sandbox, ValidateStripePublishableKeyPosture(cfg, "pk_test_abc") == nil)
		require.Equal(t, !sandbox, ValidateStripePublishableKeyPosture(cfg, "pk_live_abc") == nil)
		require.Error(t, ValidateStripePublishableKeyPosture(cfg, "sk_test_abc"))
		require.Error(t, ValidateStripePublishableKeyPosture(cfg, "pk_test_"))
	}
}

func TestRailSetShapeValidation(t *testing.T) {
	nmi := func(secret string) *PSPConfig {
		return &PSPConfig{Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec", WebhookSigningSecret: secret}}
	}
	ccbill := func(accountID string) *PSPConfig {
		return &PSPConfig{Rail: models.RailCCBill, AccountID: accountID, CCBill: &CCBillRailConfig{Salt: "s"}}
	}
	solana := func(provider, key string) *PSPConfig {
		return &PSPConfig{Rail: models.RailSolana, Solana: &SolanaRailConfig{RPCProvider: provider, RPCAPIKey: key}}
	}
	withAccount := func(p *PSPConfig, id string) *PSPConfig { p.AccountID = id; return p }
	custody := func(rail *PSPConfig, c CustodianConfig) *PSPConfig { rail.Custody = &c; return rail }
	bt := CustodianConfig{Custodian: models.CustodianBasisTheory, AccountID: "tnt_1", APIKey: "key"}

	for name, row := range map[string]struct {
		rails PSPSet
		want  string
	}{
		"reserved name infers rail":   {PSPSet{"stripe": {Stripe: &StripeRailConfig{SecretKey: "sk_test_x", WebhookSigningSecret: "wh"}}}, ""},
		"custom name needs rail":      {PSPSet{"mobius": {NMI: &NMIRailConfig{SecurityKey: "s", WebhookSigningSecret: "w"}}}, "unknown type"},
		"wrong typed block":           {PSPSet{"stripe": {Rail: models.RailStripe, NMI: &NMIRailConfig{SecurityKey: "sec"}}}, "type stripe must use stripe block"},
		"two typed blocks":            {PSPSet{"p": {Rail: models.RailNMI, NMI: &NMIRailConfig{}, Stripe: &StripeRailConfig{}}}, "exactly one provider block"},
		"nmi webhook secret":          {PSPSet{"mobius": nmi("")}, "webhook_signing_secret is required"},
		"stripe webhook secret":       {PSPSet{"stripe": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_test_x"}}}, "webhook_signing_secret is required"},
		"nmi security key":            {PSPSet{"mobius": {Rail: models.RailNMI, NMI: &NMIRailConfig{WebhookSigningSecret: "w"}}}, "security_key is required"},
		"two stripe without ids":      {PSPSet{"a": stripeRail("sk_test_a"), "b": stripeRail("sk_test_b")}, "must declare account_id"},
		"two stripe with ids":         {PSPSet{"a": withAccount(stripeRail("sk_test_a"), "acct_a"), "b": withAccount(stripeRail("sk_test_b"), "acct_b")}, ""},
		"archived still needs id":     {PSPSet{"a": func() *PSPConfig { p := stripeRail("sk_test_a"); p.Archived = true; return p }(), "b": stripeRail("sk_test_b")}, "must declare account_id"},
		"two solana need no id":       {PSPSet{"a": solana("helius", "k"), "b": solana("public", "")}, ""},
		"ccbill dash identity":        {PSPSet{"ccbill": ccbill("945280-0000")}, ""},
		"ccbill slash identity":       {PSPSet{"ccbill": ccbill("945280/0000")}, "CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000"},
		"ccbill missing identity":     {PSPSet{"ccbill": ccbill("")}, "account_id is required"},
		"ccbill half datalink":        {PSPSet{"ccbill": {Rail: models.RailCCBill, AccountID: "1-2", CCBill: &CCBillRailConfig{Salt: "s", DataLinkUsername: "u"}}}, "datalink_username and datalink_password"},
		"ccbill without salt":         {PSPSet{"ccbill": {Rail: models.RailCCBill, AccountID: "1-2", CCBill: &CCBillRailConfig{}}}, "salt is required"},
		"solana default provider":     {PSPSet{"solana": solana("", "k")}, ""},
		"solana unknown provider":     {PSPSet{"solana": solana("quicknode", "k")}, "rpc_provider must be helius or public"},
		"solana public with key":      {PSPSet{"solana": solana("public", "k")}, "rpc_provider public cannot use rpc_api_key"},
		"custody on proxy rail":       {PSPSet{"mobius": custody(nmi("w"), bt)}, ""},
		"custody psp is no custodian": {PSPSet{"stripe": custody(stripeRail("sk_test_x"), CustodianConfig{Custodian: models.CustodianPSP})}, ""},
		"custody on non-proxy rail":   {PSPSet{"stripe": custody(stripeRail("sk_test_x"), bt)}, "not supported on this rail"},
		"custody without tenant":      {PSPSet{"mobius": custody(nmi("w"), CustodianConfig{Custodian: models.CustodianBasisTheory, APIKey: "k"})}, "requires account_id"},
		"custody without api key":     {PSPSet{"mobius": custody(nmi("w"), CustodianConfig{Custodian: models.CustodianBasisTheory, AccountID: "t"})}, "requires the api_key secret"},
		"custody unknown kind":        {PSPSet{"mobius": custody(nmi("w"), CustodianConfig{Custodian: "vaultco", AccountID: "t", APIKey: "k"})}, "vaultco"},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateRailSet(postureConfig(true), row.rails)
			if row.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, row.want)
			}
		})
	}

	derived := ccbill("945280-0000").ToCCBillConfig()
	require.Equal(t, [3]string{"945280", "0000", "s"}, [3]string{derived.ClientAccNum, derived.ClientSubAcc, derived.Salt})
	require.Empty(t, ccbill("945280").ToCCBillConfig().ClientAccNum, "a malformed identity never half-derives")
}

// Config-only selection is deterministic (sorted keys) and never picks an
// archived account for new work.
func TestRailSelectors(t *testing.T) {
	rails := PSPSet{
		"stripe_old": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_live_old"}},
		"stripe_new": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_new"}},
		"stripe_zed": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_zed"}},
		"mobius":     {Rail: models.RailNMI, AccountID: "100001", NMI: &NMIRailConfig{SecurityKey: "a"}},
		"paykings":   {Rail: models.RailNMI, AccountID: "100002", NMI: &NMIRailConfig{SecurityKey: "b"}},
		"old-nmi":    {Rail: models.RailNMI, AccountID: "100003", Archived: true, NMI: &NMIRailConfig{SecurityKey: "c"}},
	}
	key, proc, err := rails.ActiveRailByType(models.RailStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Same(t, proc, rails.GetStripeRail())
	require.Equal(t, []string{"mobius", "paykings"}, rails.ActiveRailKeysByType(models.RailNMI))
	require.Nil(t, rails.GetSolanaRail())

	got, ok := rails.FindByAccountID(models.RailNMI, " 100003 ")
	require.True(t, ok, "archived accounts stay addressable for inbound events")
	require.Equal(t, "c", got.NMI.SecurityKey)
	for _, id := range []string{"999", "", "100001x"} {
		_, ok = rails.FindByAccountID(models.RailNMI, id)
		require.False(t, ok, id)
	}
	_, ok = rails.FindByAccountID(models.RailStripe, "100001")
	require.False(t, ok, "account ids are scoped to their rail")
}
