package config

import (
	"strconv"
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

// stripeTestModeConfig builds a minimal Config plus in-memory PSPSet with a
// single Stripe rail carrying the given secret key and test_mode setting.
func stripeTestModeConfig(secretKey string, testMode bool) (*Config, PSPSet) {
	posture := CredentialPostureLive
	if testMode {
		posture = CredentialPostureSandbox
	}
	return &Config{
			// SEC-18: dev posture is declared, never inferred from an empty Env
			ProviderWriteMode: ProviderWriteModeFull,
			TestMode:          posture,
		}, PSPSet{
			"stripe": {
				Rail:   models.RailStripe,
				Stripe: &StripeRailConfig{SecretKey: secretKey},
			},
		}
}

func TestValidateStripeKeyForTestMode(t *testing.T) {
	// Restricted keys and secret keys follow the same credential-posture rules,
	// including outside development. Refusal must not rewrite credentials.
	for _, prefix := range []string{"sk", "rk"} {
		for _, env := range []string{"development", "production"} {
			for _, keyMode := range []string{"test", "live"} {
				for _, sandbox := range []bool{true, false} {
					key := prefix + "_" + keyMode + "_abc123"
					t.Run(env+"/"+key+"/"+strconv.FormatBool(sandbox), func(t *testing.T) {
						cfg, rails := stripeTestModeConfig(key, sandbox)

						err := validateStripeKeyForTestMode(cfg, rails)
						if sandbox == (keyMode == "test") {
							require.NoError(t, err)
						} else {
							require.Error(t, err)
						}
						require.Equal(t, key, rails["stripe"].Stripe.SecretKey)
					})
				}
			}
		}
	}
	cfg, rails := stripeTestModeConfig("sk_live_primary", false)
	rails["stripe_archived"] = &PSPConfig{Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_test_legacy", WebhookSigningSecret: "whsec_fixture"}}
	require.Error(t, validateStripeKeyForTestMode(cfg, rails), "archived accounts must also match the declared posture")
	require.Equal(t, "sk_live_primary", rails["stripe"].Stripe.SecretKey)
	require.Equal(t, "sk_test_legacy", rails["stripe_archived"].Stripe.SecretKey)
}

func TestActiveRailByType(t *testing.T) {
	rails := PSPSet{
		"stripe_old": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_live_old", WebhookSigningSecret: "whsec_fixture"}},
		"stripe_new": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_new", WebhookSigningSecret: "whsec_fixture"}},
		"mobius":     {Rail: models.RailNMI, NMI: &NMIRailConfig{SecurityKey: "sec"}},
	}
	key, proc, err := rails.ActiveRailByType(models.RailStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.Stripe.SecretKey)
	require.Equal(t, proc, rails.GetStripeRail())

	rails["stripe_other"] = &PSPConfig{Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_other", WebhookSigningSecret: "whsec_fixture"}}
	key, proc, err = rails.ActiveRailByType(models.RailStripe)
	require.NoError(t, err)
	require.Equal(t, "stripe_new", key)
	require.Equal(t, "sk_live_new", proc.Stripe.SecretKey)
	require.NoError(t, ValidateRailSet(&Config{ProviderWriteMode: ProviderWriteModeFull}, PSPSet{
		"stripe_a": {Rail: models.RailStripe, AccountID: "acct_a", Stripe: &StripeRailConfig{SecretKey: "sk_live_a", WebhookSigningSecret: "whsec_fixture"}},
		"stripe_b": {Rail: models.RailStripe, AccountID: "acct_b", Stripe: &StripeRailConfig{SecretKey: "sk_live_b", WebhookSigningSecret: "whsec_fixture"}},
	}))

	// Two accounts on a rail without account_id is rejected: the made-up map name
	// can't be the provider identity (#641).
	require.ErrorContains(t, ValidateRailSet(&Config{ProviderWriteMode: ProviderWriteModeFull}, PSPSet{
		"stripe_a": {Rail: models.RailStripe, Archived: true, Stripe: &StripeRailConfig{SecretKey: "sk_live_a", WebhookSigningSecret: "whsec_fixture"}},
		"stripe_b": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_b", WebhookSigningSecret: "whsec_fixture"}},
	}), "must declare account_id")
}

// TestCatalogTargetSelectors covers active account selection plus
// FindByAccountID (resolve a specific account by gateway-id).
func TestCatalogTargetSelectors(t *testing.T) {
	rails := PSPSet{
		"mobius":   {Rail: models.RailNMI, AccountID: "100001", NMI: &NMIRailConfig{SecurityKey: "a"}},
		"paykings": {Rail: models.RailNMI, AccountID: "100002", NMI: &NMIRailConfig{SecurityKey: "b"}},
		"old-nmi":  {Rail: models.RailNMI, AccountID: "100003", Archived: true, NMI: &NMIRailConfig{SecurityKey: "c"}},
	}

	pk, primary, err := rails.ActiveRailByType(models.RailNMI)
	require.NoError(t, err)
	require.Equal(t, "mobius", pk)
	require.Equal(t, "100001", primary.AccountID)

	// Non-archived accounts are catalog-sync targets; archived is excluded.
	require.Equal(t, []string{"mobius", "paykings"}, rails.ActiveRailKeysByType(models.RailNMI))

	// FindByAccountID resolves by operator-declared gateway-id.
	got, ok := rails.FindByAccountID(models.RailNMI, "100002")
	require.True(t, ok)
	require.Equal(t, "b", got.NMI.SecurityKey)
	_, ok = rails.FindByAccountID(models.RailNMI, "999")
	require.False(t, ok)
}

// TestRailEnvironmentDerivesFromTestMode covers #882: a PSP has NO environment
// axis to contradict — test_mode alone decides, and every PSP in a deployment
// therefore lands in the same environment.
func TestRailEnvironmentDerivesFromTestMode(t *testing.T) {
	require.Equal(t, ProviderEnvironmentTest, ExpectedProviderEnvironment(true))
	require.Equal(t, ProviderEnvironmentLive, ExpectedProviderEnvironment(false))

	sandbox := &Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: CredentialPostureSandbox}
	require.Equal(t, ProviderEnvironmentTest, ExpectedProviderEnvironment(sandbox.IsTestMode()))
	require.NoError(t, ValidateRailSet(sandbox, PSPSet{
		"stripe": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_test_x", WebhookSigningSecret: "whsec_fixture"}},
	}))

	live := &Config{ProviderWriteMode: ProviderWriteModeFull, TestMode: CredentialPostureLive}
	require.Equal(t, ProviderEnvironmentLive, ExpectedProviderEnvironment(live.IsTestMode()))
	require.NoError(t, ValidateRailSet(live, PSPSet{
		"stripe": {Rail: models.RailStripe, Stripe: &StripeRailConfig{SecretKey: "sk_live_x", WebhookSigningSecret: "whsec_fixture"}},
	}))
}

func TestStripeLiveKeyRejectedInTestMode(t *testing.T) {
	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.TestMode = CredentialPostureSandbox
	rails := PSPSet{
		"stripe": {Rail: "stripe", Stripe: &StripeRailConfig{SecretKey: "sk_live_abc123", WebhookSigningSecret: "whsec_fixture"}},
	}
	require.Error(t, ValidateRailSet(cfg, rails))

	// test key in the test env is fine
	rails["stripe"].Stripe.SecretKey = "sk_test_abc123"
	require.NoError(t, ValidateRailSet(cfg, rails))

	// test key with live credentials expected: hard error without mutation
	cfg2 := GetDefaultBillingConfig()
	cfg2.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg2.ProviderWriteMode = ProviderWriteModeLimited
	rails2 := PSPSet{
		"stripe": {
			Rail:   "stripe",
			Stripe: &StripeRailConfig{SecretKey: "sk_test_abc123", WebhookSigningSecret: "whsec_fixture"},
		},
	}
	require.Error(t, ValidateRailSet(cfg2, rails2))
	require.Equal(t, "sk_test_abc123", rails2["stripe"].Stripe.SecretKey)
}

func TestRailConfigTypedBlocksAndArchived(t *testing.T) {
	cfg := &Config{}
	rails := PSPSet{
		"mobius": {
			Rail: models.RailNMI,
			NMI: &NMIRailConfig{
				SecurityKey:          "sec",
				WebhookSigningSecret: "wh",
			},
		},
		"stripe_old": {
			Rail:     models.RailStripe,
			Archived: true,
			Stripe:   &StripeRailConfig{SecretKey: "sk_live_old", WebhookSigningSecret: "whsec_fixture"},
		},
	}
	require.NoError(t, ValidateRailSet(cfg, rails))
	require.Equal(t, "sec", rails["mobius"].NMI.SecurityKey)
	require.Equal(t, "sk_live_old", rails["stripe_old"].Stripe.SecretKey)
	require.True(t, rails["stripe_old"].Archived)
}

func TestRailConfigRejectsWrongTypedBlock(t *testing.T) {
	err := ValidateRailSet(&Config{}, PSPSet{
		"stripe": {Rail: models.RailStripe, NMI: &NMIRailConfig{SecurityKey: "sec"}},
	})
	require.ErrorContains(t, err, "type stripe must use stripe block")
}

// #697: CCBill composite identity is dash-joined; slash-form account ids are
// rejected loudly (even in dev — a format bug, not a missing credential).
func TestCCBillAccountIDRejectsSlash(t *testing.T) {
	ccbillBlock := &CCBillRailConfig{Salt: "s"}
	err := ValidateRailSet(&Config{}, PSPSet{
		"ccbill": {Rail: models.RailCCBill, AccountID: "945280/0000", CCBill: ccbillBlock},
	})
	require.ErrorContains(t, err, "CCBill account_id uses a dash: clientAccnum-clientSubacc, e.g. 945280-0000")

	// The dash form passes.
	require.NoError(t, ValidateRailSet(&Config{}, PSPSet{
		"ccbill": {Rail: models.RailCCBill, AccountID: "945280-0000", CCBill: ccbillBlock},
	}))

	// #711: identity is declared ONCE — the clientAccnum/clientSubacc pair
	// derives from the dash-joined account_id; an empty account_id is rejected
	// even in dev.
	err = ValidateRailSet(&Config{}, PSPSet{
		"ccbill": {Rail: models.RailCCBill, CCBill: ccbillBlock},
	})
	require.ErrorContains(t, err, "account_id is required")

	derived := (&PSPConfig{Rail: models.RailCCBill, AccountID: "945280-0000", CCBill: ccbillBlock}).ToCCBillConfig()
	require.Equal(t, "945280", derived.ClientAccNum)
	require.Equal(t, "0000", derived.ClientSubAcc)
	require.Equal(t, "s", derived.Salt)
}

func TestSolanaRPCProviderValidation(t *testing.T) {
	cfg := GetDefaultBillingConfig()

	base := func(solana *SolanaRailConfig) PSPSet {
		return PSPSet{"solana": {Rail: models.RailSolana, Solana: solana}}
	}

	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "helius", RPCAPIKey: "key"})))
	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "", RPCAPIKey: "key"})))
	require.NoError(t, ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "public"})))

	err := ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "quicknode", RPCAPIKey: "key"}))
	require.ErrorContains(t, err, "rpc_provider must be helius or public")

	err = ValidateRailSet(cfg, base(&SolanaRailConfig{RPCProvider: "public", RPCAPIKey: "key"}))
	require.ErrorContains(t, err, "rpc_provider public cannot use rpc_api_key")
}

// TestWebhookSecretRequiredOutsideDev verifies that a missing webhook_signing_secret is a
// hard boot error in production but only a warning in development.
func TestWebhookSecretRequiredOutsideDev(t *testing.T) {
	for _, rail := range []models.Rail{models.RailStripe, models.RailNMI} {
		for _, env := range []string{"development", "production"} {
			for _, secret := range []string{"", "whsec_test_dummy"} {
				t.Run(string(rail)+"/"+env+"/"+secret, func(t *testing.T) {
					cfg := GetDefaultBillingConfig()

					if env == "development" {
						cfg.TestMode = CredentialPostureSandbox
					}
					psp := &PSPConfig{Rail: rail}
					if rail == models.RailStripe {
						key := "sk_live_dummy"
						if env == "development" {
							key = "sk_test_dummy"
						}
						psp.Stripe = &StripeRailConfig{SecretKey: key, WebhookSigningSecret: secret}
					} else {
						psp.NMI = &NMIRailConfig{SecurityKey: "sec_dummy", WebhookSigningSecret: secret}
					}
					err := ValidateRailSet(cfg, PSPSet{"p": psp})
					if secret == "" {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				})
			}
		}
	}
}
