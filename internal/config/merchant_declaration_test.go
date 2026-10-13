package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A typo'd key must fail loudly instead of declaring a merchant with no PSPs,
// and the document names its merchant.
func TestParseMerchantDeclarationIsStrict(t *testing.T) {
	m, err := ParseMerchantDeclaration([]byte(`
slug: Host-One
display_name: Host One
sandbox: true
psps:
  mobius:
    rail: nmi
    environment: test
    gateway_id: "100001"
    secrets: {security_key: sk, webhook_signing_secret: whs}
`))
	require.NoError(t, err)
	require.Equal(t, "host-one", m.Slug)
	require.Equal(t, "100001", m.PSPs["mobius"].AccountID)

	for doc, want := range map[string]string{
		"slug: x\ndisplay_name: X\nacounts: {}\n":                "acounts",
		"slug: x\ndisplay_name: X\nrail_merchant_accounts: {}\n": "rail_merchant_accounts was renamed to psps",
		"slug: x\ndisplay_name: X\nprovider_accounts: {}\n":      "provider_accounts was renamed to psps",
		"slug: x\ndisplay_name: X\napi_host: api.x.example\n":    "api_host was removed",
		"display_name: X\n":              "slug is required",
		"slug: \" \"\ndisplay_name: X\n": "slug is required",
		"slug: -x-\ndisplay_name: X\n":   "invalid merchant slug",
	} {
		_, err := ParseMerchantDeclaration([]byte(doc))
		require.ErrorContains(t, err, want)
	}
}

const treasury = "CXFYwCTBtzsF3qS5zZyWqMKYwBVFHUHCrtTiJkAa5c1w"

// A PSP's rail decides its fields: every rail refuses a field or secret it
// does not take, naming the PSP, the field and what the rail does take.
func TestParseMerchantDeclarationRefusesUnknownPSPKeys(t *testing.T) {
	doc := func(psp string) string { return "slug: x\ndisplay_name: X\npsps:\n  p: " + psp + "\n" }
	nmi := "(nmi takes rail, environment, archived, gateway_id, custodian, tokenization_key, tokenization_url, card_entry, webhook_overlap_expires_at, secrets)"
	solana := "(solana takes rail, environment, archived, treasury, secrets)"
	for psp, want := range map[string]string{
		`{rail: nmi, gateway_id: "1", tokenisation_key: tk}`:                                   `psps.p: a nmi PSP has no "tokenisation_key" ` + nmi,
		`{rail: nmi, gateway_id: "1", settings: {tokenization_key: tk}}`:                       `psps.p: a nmi PSP has no "settings" ` + nmi,
		`{rail: nmi, gateway_id: "1", Tokenization_Key: tk, cardentry: x}`:                     `psps.p: a nmi PSP has no "Tokenization_Key", "cardentry"`,
		`{rail: nmi, gateway_id: "1", secrets: {security_kye: sk}}`:                            `psps.p.secrets: unknown field "security_kye" (nmi takes security_key, webhook_signing_secret, webhook_signing_secret_previous)`,
		`{rail: stripe, account_id: acct_1, publishablekey: pk_1}`:                             `psps.p: a stripe PSP has no "publishablekey" (stripe takes rail, environment, archived, account_id, publishable_key, webhook_overlap_expires_at, secrets)`,
		`{rail: stripe, account_id: acct_1, custodian: bt}`:                                    `psps.p: a stripe PSP has no "custodian"`,
		`{rail: ccbill, client_accnum: "999999", client_subacc: "0000", tokenization_key: tk}`: `psps.p: a ccbill PSP has no "tokenization_key" (ccbill takes rail, environment, archived, client_accnum, client_subacc, secrets)`,
		`{rail: solana, treasury: ` + treasury + `, tokens: {USDC: {}}}`:                       `psps.p: a solana PSP has no "tokens" ` + solana,
		`{rail: solana, treasury: ` + treasury + `, rpc_provider: helius}`:                     `psps.p: a solana PSP has no "rpc_provider" ` + solana,
		`{rail: solana, treasury: ` + treasury + `, recipient_wallet: w}`:                      `psps.p: a solana PSP has no "recipient_wallet" ` + solana,
		`{rail: solana, treasury: ` + treasury + `, signer: {mode: x}}`:                        `psps.p: a solana PSP has no "signer" ` + solana,
		`{rail: solana, account_id: ` + treasury + `}`:                                         `psps.p: a solana PSP has no "account_id" ` + solana,
		`{rail: solana, treasury: ` + treasury + `, secrets: {private_key: k}}`:                `psps.p.secrets: unknown field "private_key" (solana takes rpc_url)`,
		`{rail: solana}`:                                               `psps.p.treasury is required`,
		`{rail: nmi, account_id: "1"}`:                                 `psps.p: a nmi PSP has no "account_id" ` + nmi,
		`{rail: nmi}`:                                                  `psps.p.gateway_id is required`,
		`{rail: ccbill, account_id: "999999-0000"}`:                    `psps.p: a ccbill PSP has no "account_id"`,
		`{rail: ccbill, client_accnum: "999999"}`:                      `psps.p.client_subacc is required`,
		`{rail: ccbill, client_accnum: "999999", client_subacc: 0000}`: `psps.p: client_subacc must be a string: quote it (client_subacc: "0")`,
		`{rail: stripe}`:                                               `psps.p.account_id is required`,
		`{rail: solana, treasury: nope}`:                               `psps.p: treasury "nope" is not a Solana address`,
		`{rail: nmii, gateway_id: "1"}`:                                `psps.p: unknown rail "nmii" (ccbill, nmi, solana, stripe)`,
		`{gateway_id: "1"}`:                                            `psps.p: rail is required (ccbill, nmi, solana, stripe)`,
	} {
		_, err := ParseMerchantDeclaration([]byte(doc(psp)))
		require.ErrorContains(t, err, want, psp)
	}

	// Secret keys match as the secret store reads them.
	_, err := ParseMerchantDeclaration([]byte(doc(`{rail: nmi, environment: live, gateway_id: "1", secrets: {Security_Key: sk}}`)))
	require.NoError(t, err)
}

// A PSP's environment is its rail's own, its real-money one when omitted,
// and it agrees with its merchant: a live merchant's PSPs move real money, a
// sandbox's none.
func TestParseMerchantDeclarationEnvironments(t *testing.T) {
	doc := func(psps string) string { return "slug: x\ndisplay_name: X\npsps:\n" + psps }
	sandbox := func(psps string) string { return "slug: x\ndisplay_name: X\nsandbox: true\npsps:\n" + psps }
	for psps, want := range map[string]string{
		"  p: {rail: stripe, environment: sandbox, account_id: acct_1}\n":                                             `psps.p.environment: stripe has no "sandbox" environment (live, test)`,
		"  p: {rail: ccbill, environment: test, client_accnum: \"999999\", client_subacc: \"0000\"}\n":                `psps.p.environment: ccbill has no "test" environment (live, sandbox)`,
		"  p: {rail: solana, environment: sandbox, treasury: " + treasury + "}\n":                                     `psps.p.environment: solana has no "sandbox" environment (mainnet, devnet, testnet)`,
		"  p: {rail: solana, environment: live, treasury: " + treasury + "}\n":                                        `psps.p.environment: solana has no "live" environment (mainnet, devnet, testnet)`,
		"  p: {rail: nmi, environment: devnet, gateway_id: \"1\"}\n":                                                  `psps.p.environment: nmi has no "devnet" environment (live, test, sandbox)`,
		"  p: {rail: nmi, gateway_id: \"1\"}\n  q: {rail: solana, environment: devnet, treasury: " + treasury + "}\n": "psps.q.environment: devnet moves no money, and the merchant is live; a sandbox is its own merchant, declared sandbox: true",
	} {
		_, err := ParseMerchantDeclaration([]byte(doc(psps)))
		require.ErrorContains(t, err, want, psps)
	}
	// A sandbox merchant's PSPs move no money.
	_, err := ParseMerchantDeclaration([]byte(sandbox("  p: {rail: solana, treasury: " + treasury + "}\n")))
	require.ErrorContains(t, err, "psps.p.environment: mainnet moves real money, and the merchant is a sandbox (sandbox: true); its PSPs take devnet or testnet")
	m, err := ParseMerchantDeclaration([]byte(sandbox("  mobius: {rail: nmi, environment: sandbox, gateway_id: \"1\"}\n  wallet: {rail: solana, environment: devnet, treasury: " + treasury + "}\n  ccbill: {rail: ccbill, environment: sandbox, client_accnum: \"999999\", client_subacc: \"0000\"}\n")))
	require.NoError(t, err)
	require.True(t, m.Sandbox)
	require.Equal(t, EnvironmentDevnet, m.PSPs["wallet"].Environment)
	require.Equal(t, treasury, m.PSPs["wallet"].AccountID)

	m, err = ParseMerchantDeclaration([]byte(doc("  mobius: {rail: nmi, gateway_id: \"1\"}\n  wallet: {rail: solana, treasury: " + treasury + "}\n")))
	require.NoError(t, err)
	require.Equal(t, EnvironmentLive, m.PSPs["mobius"].Environment, "omitted is the rail's real-money environment")
	require.Equal(t, EnvironmentMainnet, m.PSPs["wallet"].Environment, "omitted is the rail's real-money environment")
}
