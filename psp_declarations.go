package openrails

import (
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// The typed PSP declarations: one struct per rail whose fields are the rail's
// slots, so a misspelled secret or setting fails to compile. Each field's psp
// tag names the declaration key it fills. PSPConfig returns the same value the
// YAML parses to; an empty field is an omitted key. Environment is the rail's
// own; omitted, it is the rail's real-money one (live, or Solana's mainnet).
//
// A slot tagged required is not checked here: as in YAML, a PSP declared
// without one exists but is not armed until the credential is supplied.

// NMIPSP declares a PSP on the nmi rail.
type NMIPSP struct {
	// GatewayID is the NMI dashboard's Gateway ID: the PSP's account.
	GatewayID string `psp:"gateway_id"`
	// Environment is live (the default), test (a gateway account in test
	// mode) or sandbox (a sandbox.nmi.com account).
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
	WebhookOverlapExpiresAt      time.Time `psp:"webhook_overlap_expires_at"`

	// TokenizationKey is the public Collect.js key the browser's card fields use.
	TokenizationKey string `psp:"tokenization_key"`
	// TokenizationURL overrides the Collect.js script URL.
	TokenizationURL string `psp:"tokenization_url"`
	// CardEntry is browser (the default) or server, where the payment page
	// posts the card to OpenRails, which vaults it.
	CardEntry string `psp:"card_entry"`
}

// PSPConfig returns the declaration.
func (p NMIPSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailNMI, Environment: p.Environment, AccountID: p.GatewayID, Archived: p.Archived, Custodian: p.Custodian,
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
		}),
	}
}

// StripePSP declares a PSP on the stripe rail.
type StripePSP struct {
	// AccountID is the Stripe account, acct_….
	AccountID string `psp:"account_id"`
	// Environment is live (the default) or test, as the secret key's prefix
	// says.
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`

	SecretKey            string `psp:"secrets.secret_key,required"`
	WebhookSigningSecret string `psp:"secrets.webhook_signing_secret,required"`
	// WebhookSigningSecretThin verifies the thin-event endpoint.
	WebhookSigningSecretThin string `psp:"secrets.webhook_signing_secret_thin"`
	// WebhookSigningSecretPrevious is the rotated-out secret. It verifies only
	// until WebhookOverlapExpiresAt, which must be set with it.
	WebhookSigningSecretPrevious string    `psp:"secrets.webhook_signing_secret_previous"`
	WebhookOverlapExpiresAt      time.Time `psp:"webhook_overlap_expires_at"`

	// PublishableKey (pk_…) enables embedded Elements; without it purchase
	// redirects to Stripe Checkout.
	PublishableKey string `psp:"publishable_key"`
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
		}),
	}
}

// CCBillPSP declares a PSP on the ccbill rail.
type CCBillPSP struct {
	// ClientAccnum and ClientSubacc are the CCBill account and subaccount
	// numbers, like 945280 and 0000: the PSP's account.
	ClientAccnum string `psp:"client_accnum"`
	ClientSubacc string `psp:"client_subacc"`
	// Environment is live (the default) or sandbox (CCBill's sandbox).
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`

	// Salt signs FlexForm links and verifies their callbacks.
	Salt             string `psp:"secrets.salt,required"`
	DatalinkUsername string `psp:"secrets.datalink_username"`
	DatalinkPassword string `psp:"secrets.datalink_password"`
}

// PSPConfig returns the declaration.
func (p CCBillPSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailCCBill, Environment: p.Environment, Archived: p.Archived,
		AccountID: rails.Account(models.RailCCBill, map[string]string{"client_accnum": p.ClientAccnum, "client_subacc": p.ClientSubacc}),
		Secrets: secretSlots(map[string]string{
			"salt":              p.Salt,
			"datalink_username": p.DatalinkUsername,
			"datalink_password": p.DatalinkPassword,
		}),
	}
}

// SolanaPSP declares a Solana PSP: the merchant's treasury on the network its
// environment names. It holds no key: the deployment's cranker
// (Config.Solana.Crankers) publishes plans and pulls renewals into the
// treasury.
type SolanaPSP struct {
	// Environment is mainnet (the default), devnet or testnet.
	Environment string `psp:"environment"`
	Archived    bool   `psp:"archived"`
	// Treasury is the Solana address every payment to the merchant lands in:
	// the PSP's identity.
	Treasury string `psp:"treasury"`
	// RPCURL is the PSP's own Solana JSON-RPC endpoint, tried before the
	// network's public one. A secret: its URL usually carries the provider's
	// key.
	RPCURL string `psp:"secrets.rpc_url"`
}

// PSPConfig returns the declaration.
func (p SolanaPSP) PSPConfig() PSPConfig {
	return PSPConfig{
		Rail: billing.RailSolana, Environment: p.Environment, AccountID: p.Treasury, Archived: p.Archived,
		Secrets: secretSlots(map[string]string{"rpc_url": p.RPCURL}),
	}
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

func settingSlots(slots map[string]any) map[string]any {
	for key, value := range slots {
		if m, isMap := value.(map[string]any); value == "" || isMap && m == nil {
			delete(slots, key)
		}
	}
	if len(slots) == 0 {
		return nil
	}
	return slots
}
