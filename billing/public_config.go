package billing

// PublicConfig is what a browser needs to know about this deployment and
// its merchant, at GET /v1/config. Every value is public by nature.
type PublicConfig struct {
	// Capabilities is what the mount serving the request serves.
	Capabilities Capabilities `json:"capabilities"`
	// Currencies is the scale registry behind every amount on the wire.
	Currencies []CurrencyUnits `json:"currencies"`
	// Rails is the rail registry: each rail a PSP can be declared on, with
	// the credentials and settings it takes.
	Rails []RailDefinition `json:"rails"`
	// Payment is the merchant's browser payment setup; null when the request
	// resolves no merchant.
	Payment *PaymentConfig `json:"payment"`
	// Captcha is the challenge to solve when a request answers 403
	// captcha_required; null when the deployment challenges nobody.
	Captcha *CaptchaConfig `json:"captcha"`
}

// CaptchaConfig is how a browser solves the deployment's captcha: load
// ScriptURL, run the Provider's widget with SiteKey and Action, and resend
// the refused request with the token in the TokenHeader header.
type CaptchaConfig struct {
	Provider    string `json:"provider"`
	SiteKey     string `json:"site_key"`
	ScriptURL   string `json:"script_url"`
	Action      string `json:"action"`
	TokenHeader string `json:"token_header"`
}

// PaymentConfig is the merchant's browser-safe payment setup: its armed PSPs
// with the public values a browser needs to drive each one, and Solana's
// network and accepted tokens. It never contains merchant secrets.
type PaymentConfig struct {
	PSPs []PSPPaymentConfig `json:"psps"`
	// Solana is the network and the tokens the merchant accepts when a Solana
	// PSP is armed, else null.
	Solana *SolanaPaymentConfig `json:"solana"`
}

// SolanaPaymentConfig is the merchant's public Solana acceptance policy.
type SolanaPaymentConfig struct {
	Network        string               `json:"network"`
	Chain          string               `json:"chain"`
	PreferredToken string               `json:"preferred_token"`
	Tokens         []SolanaPaymentToken `json:"tokens"`
}

// SolanaPaymentToken is one accepted SPL token.
type SolanaPaymentToken struct {
	Symbol            string `json:"symbol"`
	Name              string `json:"name"`
	Mint              string `json:"mint"`
	Decimals          int    `json:"decimals"`
	Preferred         bool   `json:"preferred"`
	RecurringEligible bool   `json:"recurring_eligible"`
}

// PSPPaymentConfig describes one armed PSP to a browser.
type PSPPaymentConfig struct {
	// PSPID is the public stable account selector used by saved-method setup.
	PSPID PSPID `json:"psp_id"`
	// Key is the PSP's key, the checkout payment.psp selector.
	Key string `json:"key"`
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail string `json:"rail"`
	// Custodian holds the card: "psp", or the third party whose page tokenizes it.
	Custodian   string `json:"custodian"`
	DisplayName string `json:"display_name"`
	// Flow is how a browser drives this PSP: tokenize, card, elements, redirect
	// or wallet. card: the page posts the card to OpenRails, which vaults it.
	// elements: the page saves the card with the PSP's own fields and checkout
	// charges the saved card, with authentication in the page.
	Flow string `json:"flow"`
	// Checkout is true when new purchases and newly entered cards use this PSP
	// under the merchant's checkout routing. Other armed PSPs stay listed so
	// their existing cards and agreements keep working.
	Checkout bool `json:"checkout"`
	// Config holds whitelisted public values, such as a tokenization key.
	Config map[string]string `json:"config,omitempty"`
	// Status is PSPTemporarilyUnavailable when the PSP's credentials could not
	// be checked just now: it is listed without Config, and the document is
	// not cacheable. Empty is available. RetryAfter is in seconds.
	Status     string `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

// PSPTemporarilyUnavailable marks a PSP (or option) whose credentials could
// not be checked just now; retry after RetryAfter seconds.
const PSPTemporarilyUnavailable = "temporarily_unavailable"
