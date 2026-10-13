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
psps:
  mobius:
    rail: nmi
    account_id: "100001"
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

// Every rail refuses a settings or secrets key it does not take, naming the
// PSP, the key and the keys the rail does take.
func TestParseMerchantDeclarationRefusesUnknownPSPKeys(t *testing.T) {
	doc := func(psp string) string { return "slug: x\ndisplay_name: X\npsps:\n  p: " + psp + "\n" }
	nmiSettings := "(nmi takes card_entry, endpoint_deployment, fallback_after_failures, fallback_for, tokenization_key, tokenization_url, webhook_overlap_expires_at)"
	stripeSettings := "(stripe takes fallback_after_failures, fallback_for, publishable_key, webhook_overlap_expires_at)"
	for psp, want := range map[string]string{
		`{rail: nmi, account_id: "1", settings: {tokenisation_key: tk}}`:               `psps.p.settings: unknown field "tokenisation_key" ` + nmiSettings,
		`{rail: nmi, account_id: "1", settings: {Tokenization_Key: tk, cardentry: x}}`: `psps.p.settings: unknown fields "Tokenization_Key", "cardentry" ` + nmiSettings,
		`{rail: nmi, account_id: "1", secrets: {security_kye: sk}}`:                    `psps.p.secrets: unknown field "security_kye" (nmi takes security_key, webhook_signing_secret, webhook_signing_secret_previous)`,
		`{rail: nmi, account_id: "1", secrets: {tokenization_key: tk}}`:                `psps.p.secrets: unknown field "tokenization_key"`,
		`{rail: stripe, account_id: acct_1, settings: {publishablekey: pk_1}}`:         `psps.p.settings: unknown field "publishablekey" ` + stripeSettings,
		`{rail: stripe, account_id: acct_1, secrets: {secret: sk}}`:                    `psps.p.secrets: unknown field "secret" (stripe takes secret_key, webhook_signing_secret, webhook_signing_secret_previous, webhook_signing_secret_thin)`,
		`{rail: ccbill, account_id: 999999-0000, settings: {salt: s}}`:                 `psps.p.settings: unknown field "salt" (ccbill takes fallback_after_failures, fallback_for)`,
		`{rail: ccbill, account_id: 999999-0000, secrets: {datalink_user: u}}`:         `psps.p.secrets: unknown field "datalink_user" (ccbill takes datalink_password, datalink_username, salt)`,
		`{rail: solana, settings: {rpc_providr: public}}`:                              `psps.p.settings: unknown field "rpc_providr" (solana takes recipient_wallet, rpc_api_key, rpc_provider, tokens)`,
		`{rail: solana, secrets: {privatekey: k}}`:                                     `psps.p.secrets: unknown field "privatekey" (solana takes private_key)`,
		`{rail: nmii, account_id: "1"}`:                                                `psps.p.rail: unknown rail "nmii" (ccbill, nmi, solana, stripe)`,
		`{account_id: "1"}`:                                                            `psps.p.rail is required (ccbill, nmi, solana, stripe)`,
	} {
		_, err := ParseMerchantDeclaration([]byte(doc(psp)))
		require.ErrorContains(t, err, want, psp)
	}

	// Secret keys match as the secret store reads them.
	_, err := ParseMerchantDeclaration([]byte(doc(`{rail: nmi, account_id: "1", secrets: {Security_Key: sk}}`)))
	require.NoError(t, err)
}

// A fallback names other declared PSPs, and unlocks after a whole number of
// failures (2 unless declared).
func TestParseMerchantDeclarationChecksFallbacks(t *testing.T) {
	doc := func(ccbill string) string {
		return "slug: x\ndisplay_name: X\npsps:\n  mobius: {rail: nmi, account_id: \"1\"}\n  ccbill: {rail: ccbill, account_id: 999999-0000, settings: " + ccbill + "}\n"
	}
	m, err := ParseMerchantDeclaration([]byte(doc("{fallback_for: [Mobius]}")))
	require.NoError(t, err)
	fallback, ok, err := ReadPSPFallback(m.PSPs["ccbill"].Settings)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, PSPFallback{For: []string{"mobius"}, AfterFailures: 2}, fallback)
	m, err = ParseMerchantDeclaration([]byte(doc("{fallback_for: [mobius], fallback_after_failures: 3}")))
	require.NoError(t, err)
	fallback, _, err = ReadPSPFallback(m.PSPs["ccbill"].Settings)
	require.NoError(t, err)
	require.Equal(t, 3, fallback.AfterFailures)
	for settings, want := range map[string]string{
		"{fallback_for: [paykings]}":                               `"paykings" is no declared PSP`,
		"{fallback_for: [ccbill]}":                                 "no fallback for itself",
		"{fallback_for: mobius}":                                   "fallback_for lists PSP keys",
		"{fallback_for: []}":                                       "fallback_for names no PSP",
		"{fallback_for: [mobius, mobius]}":                         `fallback_for repeats "mobius"`,
		"{fallback_after_failures: 2}":                             "fallback_after_failures needs fallback_for",
		"{fallback_for: [mobius], fallback_after_failures: 0}":     "whole number from 1 to 100",
		"{fallback_for: [mobius], fallback_after_failures: 1.5}":   "whole number from 1 to 100",
		"{fallback_for: [mobius], fallback_after_failures: \"2\"}": "whole number from 1 to 100",
	} {
		_, err := ParseMerchantDeclaration([]byte(doc(settings)))
		require.ErrorContains(t, err, want, settings)
	}
}
