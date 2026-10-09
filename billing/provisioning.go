package billing

import (
	"time"

	"github.com/google/uuid"
)

// ScopeSCIM is the OAuth scope of a client-credentials access token that
// provisions the merchant's users over SCIM (/scim/v2).
const ScopeSCIM = "scim"

// ProvisioningTokenID names a provisioning token; on the wire "ptk_<uuid>".
type ProvisioningTokenID uuid.UUID

// ProvisioningTokenIDPrefix starts a ProvisioningTokenID on the wire.
const ProvisioningTokenIDPrefix = "ptk_"

// ParseProvisioningTokenID reads a ProvisioningTokenID.
func ParseProvisioningTokenID(s string) (ProvisioningTokenID, error) {
	u, err := parsePrefixedID("provisioning token", ProvisioningTokenIDPrefix, s)
	return ProvisioningTokenID(u), err
}

func (id ProvisioningTokenID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id ProvisioningTokenID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id ProvisioningTokenID) String() string {
	return formatPrefixedID(ProvisioningTokenIDPrefix, uuid.UUID(id))
}
func (id ProvisioningTokenID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *ProvisioningTokenID) UnmarshalText(b []byte) error {
	v, err := ParseProvisioningTokenID(string(b))
	*id = v
	return err
}

// ProvisioningToken is a bearer token the merchant's directory (AuthKit,
// Okta, Entra ID) presents at /scim/v2 to push its users. OpenRails keeps
// only its hash: the secret is answered once, when it is made. Declared is the
// merchant declaration's secrets.scim_token.
type ProvisioningToken struct {
	ID         ProvisioningTokenID `json:"id"`
	Name       string              `json:"name"`
	Declared   bool                `json:"declared"`
	CreatedAt  time.Time           `json:"created_at"`
	LastUsedAt *time.Time          `json:"last_used_at"`
}

// ProvisioningTokenListParams lists every provisioning token, or reads the 1
// to MaxBatchItems IDs names; unknown ones are absent.
type ProvisioningTokenListParams struct {
	IDs []ProvisioningTokenID
}

// CreateProvisioningTokenParams names a new provisioning token.
type CreateProvisioningTokenParams struct {
	Name string `json:"name"`
}

// CreatedProvisioningToken is a new provisioning token with its secret,
// answered only now.
type CreatedProvisioningToken struct {
	ProvisioningToken
	Token string `json:"token"`
}
