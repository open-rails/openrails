package billing

// MerchantDocument is a merchant's configuration as a file or a manifest
// entry holds it: its slug and the configuration beside it.
// openrails.ParseMerchantDocument reads one.
type MerchantDocument struct {
	// Slug is the merchant's name. A manifest entry's is its key, and it
	// refuses the field.
	Slug        string `yaml:"slug,omitempty"`
	DisplayName string `yaml:"display_name"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius).
	PSPs PSPs `yaml:"psps,omitempty"`
	// Custodians is the operator-declared custodian catalog, keyed by
	// custodian key. A custodian holds the card; a PSP charges
	// it. Declared once here and referenced by every PSP whose gateway those
	// cards are charged through.
	Custodians Custodians `yaml:"custodians,omitempty"`
	// MerchantSettings are the merchant's settings, beside display_name: the
	// same fields the configuration API reads and applies, validated the same
	// way. Omitted fields keep their stored values; a declared list replaces
	// the stored one.
	MerchantSettings `yaml:",inline"`
	// AlertWebhooks are where the merchant's operational alerts are posted.
	AlertWebhooks []AlertWebhookConfig `yaml:"alert_webhooks,omitempty"`
	// Messages is the merchant's messages document: which event types send
	// its customers a message, and its own templates. A document whose
	// templates do not render is refused alone. Nil sends the defaults.
	Messages *MessageSettings `yaml:"messages,omitempty"`
	// Secrets are the merchant's own credentials.
	Secrets MerchantSecrets `yaml:"secrets,omitempty"`
}

// MerchantDeclaration declares one merchant of an engine's Config.Merchants:
// its document and what only the host's code sets.
type MerchantDeclaration struct {
	MerchantDocument `yaml:",inline"`
	// Issuer binds the credentials of one issuer to this merchant: every
	// request whose Identity names it as issuer, and whose credential is
	// bound to no scope, acts on this merchant, as its customer or its
	// staff. It is the host's own auth's issuer (AuthKit's Token.Issuer)
	// where the host serves this one merchant with it: an absolute https URL
	// (http on a loopback host), declared by one merchant of an engine at
	// most. Its users are the merchant's own customers. A document never
	// carries it, since several hosts with their own issuers may share one.
	// Empty binds by scope only: a credential must be bound to the merchant's
	// group (authkit/auth Bound), whose id is the merchant's.
	Issuer string `yaml:"-"`
	// Binding selects an already adopted merchant by immutable ID and tenant.
	// Nil provisions the merchant by its slug.
	Binding *MerchantBinding `yaml:"-"`
}

// AlertWebhookConfig is one destination for the merchant's operational
// alerts. Its URL is a credential.
type AlertWebhookConfig struct {
	Name string `yaml:"name,omitempty"`
	URL  string `yaml:"url"`
	// Format is generic (the default), discord or slack.
	Format AlertWebhookFormat `yaml:"format,omitempty"`
	// Enabled defaults to true.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// MerchantSecrets are a merchant's own credentials, kept out of version
// control like a PSP's.
type MerchantSecrets struct {
	// SCIMToken is a provisioning token (at least 32 characters) the
	// merchant's directory presents at /scim/v2. OpenRails keeps its hash as
	// the merchant's declared token, replaced when this changes and removed
	// when it is removed. A token another merchant holds refuses this
	// merchant's document.
	SCIMToken string `yaml:"scim_token,omitempty"`
}

// PSPs are a merchant's declared PSPs by key.
type PSPs map[string]PSPConfig

// PSPConfig is one declared PSP: the merchant's account on a rail.
type PSPConfig struct {
	// Rail is the gateway kind: nmi, ccbill, stripe or solana.
	Rail Rail `yaml:"rail"`
	// Environment is the rail's own: nmi live | test | sandbox, stripe live |
	// test, ccbill live | sandbox, solana mainnet | devnet | testnet. Omitted
	// is the rail's real-money one. It is verified with the provider before
	// the PSP is used.
	Environment Environment `yaml:"environment,omitempty"`
	// AccountID is the card rails' own id for the account (NMI Gateway ID,
	// Stripe acct_…, CCBill clientAccnum-clientSubacc). A Solana PSP has none:
	// its account is its signer's public key.
	AccountID string `yaml:"account_id,omitempty"`
	Archived  bool   `yaml:"archived,omitempty"`
	// Custodian (nmi) references a declared custodian by key:
	// merchants.<slug>.custodians.<key>. "" = this PSP holds its own
	// instruments (Stripe pm_, NMI customer vault), which is the common case
	// and needs no configuration. A key with no matching custodian entry is a
	// hard error, never an unarmed default.
	Custodian string            `yaml:"custodian,omitempty"`
	Secrets   map[string]string `yaml:"secrets,omitempty"`
	// Settings are the rail's other non-secret fields (nmi tokenization_key,
	// stripe publishable_key, …), flat in a PSP document.
	Settings map[string]any `yaml:"settings,omitempty"`
}

// Custodians are a merchant's declared custodians by key.
type Custodians map[string]CustodianConfig

// CustodianConfig is one declared custodian: the merchant's account with a
// card custodian.
type CustodianConfig struct {
	// Kind is the vendor: basis_theory.
	Kind CardVaultKind `yaml:"kind"`
	// Environment is the tenant's money class: live (the default) or test.
	Environment Environment `yaml:"environment,omitempty"`
	// AccountID is the custodian-native tenant identity (Basis Theory: the
	// tenant id). Operator-declared: there is no runtime lookup.
	AccountID string `yaml:"account_id,omitempty"`
	// Archived drains the custodian: instruments it holds stay chargeable, no
	// new arrangement may reference it.
	Archived bool `yaml:"archived,omitempty"`
	// Settings are the declared NON-secret knobs, validated against the kind's
	// registry: public_api_key, network_tokens.
	Settings map[string]any `yaml:"settings,omitempty"`
	// Secrets are the kind's credential slots (Basis Theory: api_key, the
	// private application key), named in-process
	// custodians/<kind>/<environment>/<account_id>/<key>.
	Secrets map[string]string `yaml:"secrets,omitempty"`
}
