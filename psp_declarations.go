package openrails

import (
	"time"

	"github.com/open-rails/openrails/billing"
)

// The typed PSP declarations: one struct per rail whose fields are the rail's
// slots, so a misspelled secret or setting fails to compile. Each field's psp
// tag names the declaration key it fills. PSPConfig returns the same value the
// YAML parses to; an empty field is an omitted key. Environment is required:
// EnvironmentLive, EnvironmentTest or EnvironmentSandbox, as the rail has them.
//
// A slot tagged required is not checked here: as in YAML, a PSP declared
// without one exists but is not armed until the credential is supplied.

// NMIPSP declares a PSP on the nmi rail.
type NMIPSP struct {
	// AccountID is the NMI dashboard's "Gateway ID".
	AccountID string `psp:"account_id"`
	// Environment is live, test (a gateway account in test mode) or sandbox
	// (a sandbox.nmi.com account).
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`
	// Custodian is the key of a declared custodian holding this PSP's cards;
	// "" means NMI's customer vault holds them.
	Custodian string `psp:"custodian"`

	SecurityKey          string `psp:"secrets.security_key,required"`
	WebhookSigningSecret string `psp:"secrets.webhook_signing_secret,required"`
	// WebhookSigningSecretPrevious is the rotated-out secret. It verifies only
	// until WebhookOverlapExpiresAt, which must be set with it.
	WebhookSigningSecretPrevious string    `psp:"secrets.webhook_signing_secret_previous"`
	WebhookOverlapExpiresAt      time.Time `psp:"settings.webhook_overlap_expires_at"`

	// TokenizationKey is the public Collect.js key the browser's card fields use.
	TokenizationKey string `psp:"settings.tokenization_key"`
	// TokenizationURL overrides the Collect.js script URL.
	TokenizationURL string `psp:"settings.tokenization_url"`
	// CardEntry is browser (the default) or server, where the payment page
	// posts the card to OpenRails, which vaults it.
	CardEntry string `psp:"settings.card_entry"`

	// FallbackFor makes the PSP a fallback for the PSPs it names; see
	// CCBillPSP.FallbackFor.
	FallbackFor           []string `psp:"settings.fallback_for"`
	FallbackAfterFailures int      `psp:"settings.fallback_after_failures"`
}

// PSPConfig returns the declaration.
func (p NMIPSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailNMI, Environment: p.Environment, AccountID: p.AccountID, Archived: p.Archived, Custodian: p.Custodian,
		Secrets: secretSlots(map[string]string{
			"security_key":                    p.SecurityKey,
			"webhook_signing_secret":          p.WebhookSigningSecret,
			"webhook_signing_secret_previous": p.WebhookSigningSecretPrevious,
		}),
		Settings: settingSlots(map[string]any{
			"webhook_overlap_expires_at": instant(p.WebhookOverlapExpiresAt),
			"tokenization_key":           p.TokenizationKey,
			"tokenization_url":           p.TokenizationURL,
			"card_entry":                 p.CardEntry,
			"fallback_for":               keyList(p.FallbackFor),
			"fallback_after_failures":    count(p.FallbackAfterFailures),
		}),
	}
}

// StripePSP declares a PSP on the stripe rail.
type StripePSP struct {
	// AccountID is the Stripe account, acct_….
	AccountID string `psp:"account_id"`
	// Environment is live or test, as the secret key's prefix says.
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`

	SecretKey            string `psp:"secrets.secret_key,required"`
	WebhookSigningSecret string `psp:"secrets.webhook_signing_secret,required"`
	// WebhookSigningSecretThin verifies the thin-event endpoint.
	WebhookSigningSecretThin string `psp:"secrets.webhook_signing_secret_thin"`
	// WebhookSigningSecretPrevious is the rotated-out secret. It verifies only
	// until WebhookOverlapExpiresAt, which must be set with it.
	WebhookSigningSecretPrevious string    `psp:"secrets.webhook_signing_secret_previous"`
	WebhookOverlapExpiresAt      time.Time `psp:"settings.webhook_overlap_expires_at"`

	// PublishableKey (pk_…) enables embedded Elements; without it purchase
	// redirects to Stripe Checkout.
	PublishableKey string `psp:"settings.publishable_key"`

	// FallbackFor makes the PSP a fallback for the PSPs it names; see
	// CCBillPSP.FallbackFor.
	FallbackFor           []string `psp:"settings.fallback_for"`
	FallbackAfterFailures int      `psp:"settings.fallback_after_failures"`
}

// PSPConfig returns the declaration.
func (p StripePSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailStripe, Environment: p.Environment, AccountID: p.AccountID, Archived: p.Archived,
		Secrets: secretSlots(map[string]string{
			"secret_key":                      p.SecretKey,
			"webhook_signing_secret":          p.WebhookSigningSecret,
			"webhook_signing_secret_thin":     p.WebhookSigningSecretThin,
			"webhook_signing_secret_previous": p.WebhookSigningSecretPrevious,
		}),
		Settings: settingSlots(map[string]any{
			"webhook_overlap_expires_at": instant(p.WebhookOverlapExpiresAt),
			"publishable_key":            p.PublishableKey,
			"fallback_for":               keyList(p.FallbackFor),
			"fallback_after_failures":    count(p.FallbackAfterFailures),
		}),
	}
}

// CCBillPSP declares a PSP on the ccbill rail.
type CCBillPSP struct {
	// AccountID is clientAccnum-clientSubacc, dash-joined like 999999-0000.
	AccountID string `psp:"account_id"`
	// Environment is live or sandbox (CCBill's sandbox).
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`

	// Salt signs FlexForm links and verifies their callbacks.
	Salt             string `psp:"secrets.salt,required"`
	DatalinkUsername string `psp:"secrets.datalink_username"`
	DatalinkPassword string `psp:"secrets.datalink_password"`

	// FallbackFor makes the PSP a fallback for the PSPs it names (ccbill for
	// mobius): an order offers it only while none of them can take the
	// order, or once the order has FallbackAfterFailures failed attempts
	// through them (0: 2).
	FallbackFor           []string `psp:"settings.fallback_for"`
	FallbackAfterFailures int      `psp:"settings.fallback_after_failures"`
}

// PSPConfig returns the declaration.
func (p CCBillPSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailCCBill, Environment: p.Environment, AccountID: p.AccountID, Archived: p.Archived,
		Secrets: secretSlots(map[string]string{
			"salt":              p.Salt,
			"datalink_username": p.DatalinkUsername,
			"datalink_password": p.DatalinkPassword,
		}),
		Settings: settingSlots(map[string]any{
			"fallback_for":            keyList(p.FallbackFor),
			"fallback_after_failures": count(p.FallbackAfterFailures),
		}),
	}
}

// SolanaPSP declares the Solana wallet PSP. Its account id is the signer's
// public key, so it declares none.
type SolanaPSP struct {
	// Environment is live (mainnet) or sandbox (devnet).
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`

	// PrivateKey is the base58 key of a local keypair signer.
	PrivateKey string `psp:"secrets.private_key"`
	// TransitKey names a Vault Transit Ed25519 key that signs instead of
	// PrivateKey.
	TransitKey string `psp:"signer.key"`

	// RPCProvider is helius or public.
	RPCProvider string `psp:"settings.rpc_provider"`
	// RPCAPIKey is the Helius API key.
	RPCAPIKey string `psp:"settings.rpc_api_key"`
	// Tokens are the accepted SPL tokens by symbol.
	Tokens map[string]SolanaToken `psp:"settings.tokens"`
	// RecipientWallet is the payout address.
	RecipientWallet string `psp:"settings.recipient_wallet"`
}

// SolanaToken is one accepted SPL token. A built-in symbol (SOL, USDC, …)
// takes the network's mint and declares none.
type SolanaToken struct {
	Mint string
	Name string
}

// PSPConfig returns the declaration.
func (p SolanaPSP) PSPConfig() PSPConfig {
	out := PSPConfig{
		Rail: billing.RailSolana, Environment: p.Environment, Archived: p.Archived,
		Secrets: secretSlots(map[string]string{"private_key": p.PrivateKey}),
		Settings: settingSlots(map[string]any{
			"rpc_provider":     p.RPCProvider,
			"rpc_api_key":      p.RPCAPIKey,
			"tokens":           solanaTokens(p.Tokens),
			"recipient_wallet": p.RecipientWallet,
		}),
	}
	if p.TransitKey != "" {
		out.Signer = &PSPSignerConfig{Mode: "vault_transit", Key: p.TransitKey}
	}
	return out
}

func solanaTokens(tokens map[string]SolanaToken) map[string]any {
	if len(tokens) == 0 {
		return nil
	}
	out := make(map[string]any, len(tokens))
	for symbol, token := range tokens {
		fields := map[string]any{}
		if token.Mint != "" {
			fields["mint"] = token.Mint
		}
		if token.Name != "" {
			fields["name"] = token.Name
		}
		out[symbol] = fields
	}
	return out
}

func instant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func secretSlots(slots map[string]string) map[string]string {
	for key, value := range slots {
		if value == "" {
			delete(slots, key)
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return slots
}

// keyList is a list setting as YAML parses it; nil when empty.
func keyList(keys []string) any {
	if len(keys) == 0 {
		return nil
	}
	out := make([]any, len(keys))
	for i, key := range keys {
		out[i] = key
	}
	return out
}

// count is a whole-number setting; nil when zero.
func count(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func settingSlots(slots map[string]any) map[string]any {
	for key, value := range slots {
		if m, isMap := value.(map[string]any); value == nil || value == "" || isMap && m == nil {
			delete(slots, key)
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return slots
}
