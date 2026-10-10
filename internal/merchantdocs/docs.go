// Package merchantdocs is a merchant's configuration: everything an operator
// sets, as one document per object. It lives in a file (read into memory,
// read-only) or in HashiCorp Vault KV v2 (read and written with
// check-and-set), never in Postgres, which keeps only the identities history
// points at.
package merchantdocs

import (
	"errors"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Merchant is the merchant document: what an operator sets on the merchant
// itself.
type Merchant struct {
	DisplayName   string                   `json:"display_name,omitempty"`
	Settings      billing.MerchantSettings `json:"settings"`
	AlertWebhooks []AlertWebhook           `json:"alert_webhooks,omitempty"`
	Secrets       MerchantSecrets          `json:"secrets,omitzero"`
}

// MerchantSecrets are the merchant's own credentials.
type MerchantSecrets struct {
	// SCIMToken is the provisioning token the merchant's directory presents.
	SCIMToken string `json:"scim_token,omitempty"`
}

// AlertWebhook is a destination for the merchant's operational alerts. Its
// URL is a credential: written, never read back over the API.
type AlertWebhook struct {
	ID        billing.AlertWebhookID     `json:"id"`
	Name      string                     `json:"name,omitempty"`
	URL       string                     `json:"url"`
	Format    billing.AlertWebhookFormat `json:"format"`
	Enabled   bool                       `json:"enabled"`
	CreatedAt time.Time                  `json:"created_at"`
	UpdatedAt time.Time                  `json:"updated_at"`
}

// PSP is one PSP document: the merchant's account on a rail.
type PSP struct {
	Rail        string `json:"rail"`
	Environment string `json:"environment"`
	AccountID   string `json:"account_id"`
	Archived    bool   `json:"archived,omitempty"`
	// Custodian is the key of the custodian holding this PSP's cards; ""
	// means the PSP holds its own.
	Custodian string            `json:"custodian,omitempty"`
	Signer    *Signer           `json:"signer,omitempty"`
	Settings  map[string]any    `json:"settings,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
}

// Signer selects how a Solana PSP signs: local_keypair or vault_transit with
// the Transit key's name.
type Signer struct {
	Mode string `json:"mode,omitempty"`
	Key  string `json:"key,omitempty"`
}

// Custodian is one custodian document: the merchant's account with a card
// custodian.
type Custodian struct {
	Kind        string            `json:"kind"`
	Environment string            `json:"environment"`
	AccountID   string            `json:"account_id"`
	Archived    bool              `json:"archived,omitempty"`
	Settings    map[string]any    `json:"settings,omitempty"`
	Secrets     map[string]string `json:"secrets,omitempty"`
}

// Doc is one document with its revision: its Vault KV version, 0 in a file.
type Doc[T any] struct {
	Value     T
	Revision  int64
	UpdatedAt time.Time
}

// Set is one merchant's configuration. A merchant with no merchant document
// has HasMerchant false.
type Set struct {
	Merchant    Doc[Merchant]
	HasMerchant bool
	PSPs        map[string]Doc[PSP]
	Custodians  map[string]Doc[Custodian]
	// Rejected names each document that loaded but is not served, with why.
	Rejected map[string]string
}

// Clone copies s deeply enough that the copy's maps can change.
func (s Set) Clone() Set {
	out := s
	out.PSPs = maps.Clone(s.PSPs)
	out.Custodians = maps.Clone(s.Custodians)
	out.Rejected = maps.Clone(s.Rejected)
	if out.PSPs == nil {
		out.PSPs = map[string]Doc[PSP]{}
	}
	if out.Custodians == nil {
		out.Custodians = map[string]Doc[Custodian]{}
	}
	return out
}

// PSPKeys are the set's PSP keys in order.
func (s Set) PSPKeys() []string { return slices.Sorted(maps.Keys(s.PSPs)) }

// CustodianKeys are the set's custodian keys in order.
func (s Set) CustodianKeys() []string { return slices.Sorted(maps.Keys(s.Custodians)) }

// Revisions identifies the documents' versions, to tell a changed set from
// one loaded before.
func (s Set) Revisions() map[string]int64 {
	out := map[string]int64{}
	if s.HasMerchant {
		out[merchantDoc] = s.Merchant.Revision
	}
	for key, doc := range s.PSPs {
		out[pspDir+key] = doc.Revision
	}
	for key, doc := range s.Custodians {
		out[custodianDir+key] = doc.Revision
	}
	return out
}

const (
	merchantDoc  = "merchant"
	pspDir       = "psps/"
	custodianDir = "custodians/"
)

// MerchantDoc names the merchant document within a merchant's set.
const MerchantDoc = merchantDoc

// PSPDoc names the PSP document under key within a merchant's set.
func PSPDoc(key string) string { return pspDir + key }

// CustodianDoc names the custodian document under key within a merchant's set.
func CustodianDoc(key string) string { return custodianDir + key }

// ParseDoc splits a document name into the PSP or custodian key it holds.
func ParseDoc(name string) (psp, custodian string) {
	if key, ok := strings.CutPrefix(name, pspDir); ok {
		return key, ""
	}
	if key, ok := strings.CutPrefix(name, custodianDir); ok {
		return "", key
	}
	return "", ""
}

// KeyShape is the shape of a PSP or custodian key.
var KeyShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var (
	// ErrRevisionMismatch refuses a write naming a revision the document has
	// moved past (or a create of a document that exists).
	ErrRevisionMismatch = errors.New("merchantdocs: the document changed since the revision the write names")
	// ErrReadOnly refuses a write to configuration read from a file.
	ErrReadOnly = errors.New("merchantdocs: merchant configuration is read from a file and is read-only")
	// ErrUnavailable is an operational failure of the source (Vault
	// unreachable, sealed or refusing): retry, never read it as "absent".
	ErrUnavailable = errors.New("merchantdocs: merchant configuration is unavailable")
)
