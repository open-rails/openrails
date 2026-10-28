package billing

import (
	"time"
)

// Rail is a gateway kind: the technology a PSP runs on.
type Rail string

const (
	RailNMI    Rail = "nmi"
	RailCCBill Rail = "ccbill"
	RailStripe Rail = "stripe"
	RailSolana Rail = "solana"
)

// PSP is one merchant account on a rail: mobius and paykings are two PSPs on
// nmi. Its rail decides its fields (RailDefinition). Credential values are
// never returned.
type PSP struct {
	ID PSPID `json:"id"`
	// Key is the merchant's name for the PSP: the key a price's psps and
	// purchase's payment.rail name it by. A key names one PSP for good.
	Key  string `json:"key"`
	Rail Rail   `json:"rail"`
	// Environment is the rail's own (RailDefinition.Environments): nmi live,
	// test or sandbox; stripe live or test; ccbill live or sandbox; solana
	// mainnet, devnet or testnet. Live and mainnet move real money.
	Environment string `json:"environment"`
	// Archived PSPs take no new work and keep serving what they already
	// carry until it drains.
	Archived bool `json:"archived"`
	// OpenObligations counts the subscriptions and operations still bound to
	// the PSP; an archived PSP with none is drained.
	OpenObligations int64 `json:"open_obligations"`
	PSPFields
	Credentials map[string]PSPCredential `json:"credentials"`
	// Revision advances with every change; an update may name the revision
	// it read.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PSPAccount is a PSP's account in its rail's own terms
// (RailDefinition.AccountFields); a PSP sets only its rail's. It never
// changes.
type PSPAccount struct {
	// GatewayID is an NMI PSP's account: the dashboard's Gateway ID.
	GatewayID string `json:"gateway_id,omitempty"`
	// AccountID is a Stripe PSP's account, acct_….
	AccountID string `json:"account_id,omitempty"`
	// ClientAccnum and ClientSubacc are a CCBill PSP's account and
	// subaccount numbers.
	ClientAccnum string `json:"client_accnum,omitempty"`
	ClientSubacc string `json:"client_subacc,omitempty"`
	// Treasury is a Solana PSP's account: the address every payment to the
	// merchant lands in.
	Treasury string `json:"treasury,omitempty"`
}

// Fields are the account's fields by name.
func (a PSPAccount) Fields() map[string]string {
	return map[string]string{"gateway_id": a.GatewayID, "account_id": a.AccountID, "client_accnum": a.ClientAccnum, "client_subacc": a.ClientSubacc, "treasury": a.Treasury}
}

// PSPFields are a PSP's own non-secret fields. A rail takes its own:
// gateway_id, custodian, tokenization_key, tokenization_url, card_entry and
// webhook_overlap_expires_at on nmi; account_id, publishable_key and
// webhook_overlap_expires_at on stripe; client_accnum and client_subacc on
// ccbill; treasury on solana.
type PSPFields struct {
	PSPAccount
	// Custodian is the key of the custodian holding the PSP's cards; empty
	// when the PSP holds its own.
	Custodian string `json:"custodian,omitempty"`
	// TokenizationKey is NMI's public Collect.js key; TokenizationURL
	// overrides the script URL.
	TokenizationKey string `json:"tokenization_key,omitempty"`
	TokenizationURL string `json:"tokenization_url,omitempty"`
	// CardEntry is browser or server.
	CardEntry string `json:"card_entry,omitempty"`
	// PublishableKey is Stripe's public key for embedded Elements.
	PublishableKey string `json:"publishable_key,omitempty"`
	// WebhookOverlapExpiresAt is when a rotated-out webhook signing secret
	// stops verifying.
	WebhookOverlapExpiresAt *time.Time `json:"webhook_overlap_expires_at,omitempty"`
}

// PSPCredential is one credential slot of a PSP: its state, never its value.
type PSPCredential struct {
	Configured bool `json:"configured"`
	// ValidatedAt is when the provider last accepted the credential; null
	// when it was never checked.
	ValidatedAt *time.Time `json:"validated_at"`
}

// PSPListParams filters ListPSPs. Archived nil lists both states. IDs instead
// reads 1 to MaxBatchItems named PSPs in one page, in any state or
// environment; unknown ones are absent.
type PSPListParams struct {
	IDs      []PSPID
	Rail     Rail
	Archived *bool
	PageRequest
}

// CreatePSPParams arms a new PSP. Its rail decides its fields: its account
// fields (RailDefinition.AccountFields), and the public keys nmi
// (tokenization_key) and stripe (publishable_key) take. Environment is the
// rail's own; omitted, its real-money one. Credentials are write-only
// and are checked with the provider, against the environment, before
// anything is stored. Repeating a create that took effect returns the PSP it
// made.
type CreatePSPParams struct {
	Key         string `json:"key"`
	Rail        Rail   `json:"rail"`
	Environment string `json:"environment,omitempty"`
	// Custodian names a declared merchant-owned vault; empty keeps PSP custody.
	Custodian string `json:"custodian,omitempty"`
	PSPAccount
	TokenizationKey string            `json:"tokenization_key,omitempty"`
	PublishableKey  string            `json:"publishable_key,omitempty"`
	Credentials     map[string]string `json:"credentials"`
}

// UpdatePSPParams changes a PSP's public keys, rotates its credentials or
// archives it. ExpectedRevision, when set, is the revision the caller read: a
// PSP changed since is refused with revision_mismatch. Omitted credentials and
// keys keep their values; an empty key removes it.
type UpdatePSPParams struct {
	ExpectedRevision *int64            `json:"expected_revision"`
	TokenizationKey  *string           `json:"tokenization_key,omitempty"`
	PublishableKey   *string           `json:"publishable_key,omitempty"`
	Credentials      map[string]string `json:"credentials"`
	// Custodian changes new-card custody only; existing saved instruments and
	// accepted obligations retain their original custodian. Empty selects PSP custody.
	Custodian *string `json:"custodian,omitempty"`
	// RetireWebhookOverlap refuses the rotated-out webhook signing secret at
	// once instead of at the end of its overlap.
	RetireWebhookOverlap bool `json:"retire_webhook_overlap"`
	// Archived retires the PSP: it takes no new work and serves its
	// subscriptions until they drain; no provider call is made, and an
	// archived PSP is never restored. The last active PSP on its rail
	// archives only with AllowLast, and new purchase on the rail stops.
	Archived  bool `json:"archived"`
	AllowLast bool `json:"allow_last"`
}

// PSPDeclaration names a PSP account without credentials, for imported
// billing facts. An account already declared keeps its id, key, archive state,
// custody and credentials.
type PSPDeclaration struct {
	Key  string
	Rail Rail
	// Environment is the rail's own; omitted, its real-money one.
	Environment string
	PSPAccount
}

// RailDefinition is a rail a merchant can arm a PSP on: the fields and
// credentials a PSP on it takes over the API, and its environments.
type RailDefinition struct {
	Rail        Rail   `json:"rail"`
	DisplayName string `json:"display_name"`
	// AccountFields name a PSP's account in the rail's own terms: gateway_id
	// on nmi, account_id on stripe, client_accnum and client_subacc on
	// ccbill, treasury on solana. A create requires each.
	AccountFields []string `json:"account_fields"`
	// Fields are the other PSP fields a create takes.
	Fields         []string `json:"fields"`
	CredentialKeys []string `json:"credential_keys"`
	// Environments are the environments a PSP on the rail may declare, its
	// real-money one, the default, first.
	Environments []string `json:"environments"`
}

// PreviewPSPRoutingParams asks which PSP a purchase for a price would use.
// PSP names one explicitly (a PSP key); empty asks what the merchant's
// routing picks.
type PreviewPSPRoutingParams struct {
	PriceID PriceID `json:"price_id"`
	// Country routing is unsupported; nonempty values are rejected.
	Country string `json:"country"`
	PSP     string `json:"psp"`
}

// PSPRoutingPreview is the routing decision a purchase would get, and why
// every other PSP was passed over. Nothing is created.
type PSPRoutingPreview struct {
	// Policy is who decided: explicit, merchant or default.
	Policy string `json:"policy"`
	// Rule is the index of the merchant routing rule that matched.
	Rule *int `json:"rule"`
	// PSP is the chosen PSP's key; null when none was eligible.
	PSP        *string               `json:"psp"`
	Rail       *Rail                 `json:"rail"`
	Mode       *string               `json:"mode"`
	Candidates []PSPRoutingCandidate `json:"candidates"`
}

// PSPRoutingCandidate is one PSP routing considered.
type PSPRoutingCandidate struct {
	PSP  string `json:"psp"`
	Rail Rail   `json:"rail"`
	// Skip says why the PSP was passed over; null when it was eligible.
	Skip *string `json:"skip"`
}
