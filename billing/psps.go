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
// nmi. Credential values are never returned.
type PSP struct {
	ID PSPID `json:"id"`
	// Key is the merchant's name for the PSP: the value price psp_links and
	// checkout's payment.rail name it by. A key names one PSP for good.
	Key         string `json:"key"`
	Rail        Rail   `json:"rail"`
	Environment string `json:"environment"`
	// AccountID is the operator-declared account on the rail; it never
	// changes.
	AccountID string `json:"account_id"`
	// Archived PSPs take no new work and keep serving what they already
	// carry until it drains.
	Archived bool `json:"archived"`
	// OpenObligations counts the subscriptions and operations still bound to
	// the PSP; an archived PSP with none is drained.
	OpenObligations int64 `json:"open_obligations"`
	// Settings are the PSP's declared non-secret values, such as the public
	// keys a browser uses: publishable_key (stripe), tokenization_key (nmi).
	Settings    map[string]any           `json:"settings"`
	Credentials map[string]PSPCredential `json:"credentials"`
	// Revision advances with every change; an update may name the revision
	// it read.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// CreatePSPParams arms a new PSP. Credentials are write-only and are checked
// with the provider before anything is stored. Repeating a create that took
// effect returns the PSP it made.
type CreatePSPParams struct {
	Key         string            `json:"key"`
	Rail        Rail              `json:"rail"`
	AccountID   string            `json:"account_id"`
	Settings    map[string]any    `json:"settings"`
	Credentials map[string]string `json:"credentials"`
}

// UpdatePSPParams changes a PSP's settings, rotates its credentials or
// archives it. ExpectedRevision, when set, is the revision the caller read: a
// PSP changed since is refused with revision_mismatch. Omitted credentials and
// settings keep their values.
type UpdatePSPParams struct {
	ExpectedRevision *int64            `json:"expected_revision"`
	Settings         map[string]any    `json:"settings"`
	Credentials      map[string]string `json:"credentials"`
	// RetireWebhookOverlap refuses the rotated-out webhook signing secret at
	// once instead of at the end of its overlap.
	RetireWebhookOverlap bool `json:"retire_webhook_overlap"`
	// Archived retires the PSP: it takes no new work and serves its
	// subscriptions until they drain; no provider call is made, and an
	// archived PSP is never restored. The last active PSP on its rail
	// archives only with AllowLast, and new checkout on the rail stops.
	Archived  bool `json:"archived"`
	AllowLast bool `json:"allow_last"`
}

// PSPDeclaration names a PSP account without credentials, for imported
// billing facts. An account already declared keeps its id, key, archive state,
// custody and credentials.
type PSPDeclaration struct {
	Key       string
	Rail      Rail
	AccountID string
}

// RailDefinition is a rail a merchant can arm a PSP on, with the credential
// slots a PSP on it takes.
type RailDefinition struct {
	Rail           Rail     `json:"rail"`
	DisplayName    string   `json:"display_name"`
	CredentialKeys []string `json:"credential_keys"`
	SettingKeys    []string `json:"setting_keys"`
}

// PreviewPSPRoutingParams asks which PSP a checkout for a price would use.
// PSP names one explicitly (a PSP key); empty asks what the merchant's
// routing picks.
type PreviewPSPRoutingParams struct {
	PriceID PriceID `json:"price_id"`
	// Country is the customer's ISO-3166-1 alpha-2 country, when known.
	Country string `json:"country"`
	PSP     string `json:"psp"`
}

// PSPRoutingPreview is the routing decision a checkout would get, and why
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

// PSPRefresh reports a requested refresh of the merchant's PSPs: the pull of
// renewals, declines, cancellations and vault changes. Status is queued, or
// already_running when a refresh in flight absorbs the request.
type PSPRefresh struct {
	Status string `json:"status"`
	JobID  int64  `json:"job_id,string"`
}
