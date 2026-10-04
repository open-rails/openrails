package billing

import (
	"errors"
	"time"
)

// Control-plane vocabulary: the operations an embedded engine with
// Config.ControlPlane offers a hosted product (merchant provisioning, the
// merchant directory, fleet aggregates and retirement).

// MerchantGroupPersona and CustomerGroupPersona are the AuthKit group personas
// of merchant and customer permission groups. A customer's group ID is the
// customer's user ID.
const (
	MerchantGroupPersona = "merchant"
	CustomerGroupPersona = "customer"
)

var (
	// ErrPermissionRequired: the caller lacks the permission on the merchant.
	ErrPermissionRequired = errors.New("merchant permission required")
	// ErrMerchantUnresolved: no merchant answers to the reference.
	ErrMerchantUnresolved = errors.New("merchant identity unresolved")
	// ErrInvalidMerchantSlug wraps the slug validation detail.
	ErrInvalidMerchantSlug = errors.New("control plane provision: invalid slug")
	// ErrMerchantNameTaken: another live merchant holds the name, or holds it
	// as an unexpired former name.
	ErrMerchantNameTaken = errors.New("merchants: merchant name is taken")
	// ErrMerchantSlugReserved: a user claimed a reserved name or one outside
	// the creation pattern.
	ErrMerchantSlugReserved = errors.New("controlplane: merchant slug is reserved")
	// ErrMerchantCreationRefused is the admission gate's refusal; the
	// standard gate's reasons below wrap it.
	ErrMerchantCreationRefused               = errors.New("controlplane: merchant creation refused")
	ErrMerchantCreationEmailUnverified       = errors.New("merchant creation requires a verified email")
	ErrMerchantCreationPaymentMethodRequired = errors.New("merchant creation beyond the free allowance requires a payment method on file")
	// ErrMerchantGroupReleasePending: a retirement committed but its group
	// release has not; CompletePendingMerchantRetirements retries it.
	ErrMerchantGroupReleasePending = errors.New("merchants: retired merchant group release pending")
)

// ProvisionMerchantRequest provisions a merchant by name.
type ProvisionMerchantRequest struct {
	// Slug is the merchant name to claim.
	Slug string
	// OwnerUserID becomes the new merchant group's owner, only when this call
	// creates the merchant; an existing merchant's roles are never touched. A
	// user claim answers to the declared creation policy.
	OwnerUserID string
}

// ProvisionMerchantResult reports what ProvisionMerchant ensured.
type ProvisionMerchantResult struct {
	MerchantID MerchantID
	// GroupID is the merchant permission group's AuthKit ID; empty for an
	// existing merchant without one.
	GroupID string
	// Created reports whether this call claimed the name.
	Created bool
}

// MerchantRef is a merchant's directory identity.
type MerchantRef struct {
	ID          MerchantID `json:"id"`
	Slug        string     `json:"slug"`
	DisplayName string     `json:"display_name"`
}

// UserMerchant is a merchant a user holds a role in, with that role.
type UserMerchant struct {
	ID          MerchantID `json:"id"`
	Slug        string     `json:"slug"`
	DisplayName string     `json:"display_name"`
	Role        string     `json:"role"`
}

// FleetMerchantFunnel counts merchants by lifecycle stage: provisioned, armed
// (live PSP declared), first revenue, active in the window.
type FleetMerchantFunnel struct {
	Total         int64 `json:"total"`
	Armed         int64 `json:"armed"`
	FirstRevenue  int64 `json:"first_revenue"`
	ActiveRevenue int64 `json:"active_revenue"`
}

// FleetCurrencyRevenue is one currency's settled window volume, in the
// currency's native units as an exact decimal string.
type FleetCurrencyRevenue struct {
	Currency      string `json:"currency"`
	Payments      int64  `json:"payments"`
	SettledAmount int64  `json:"settled_amount,string"`
}

// FleetRailHealth is one rail's approved and declined charge attempts and
// chargebacks across the window.
type FleetRailHealth struct {
	Rail        string `json:"rail"`
	Succeeded   int64  `json:"succeeded"`
	Failed      int64  `json:"failed"`
	Chargebacks int64  `json:"chargebacks"`
}

// FleetMRR is one currency's monthly-normalized recurring run rate.
type FleetMRR struct {
	Currency      string `json:"currency"`
	Subscriptions int64  `json:"subscriptions"`
	MonthlyAmount int64  `json:"monthly_amount,string"`
}

// FleetSnapshot is one operator snapshot of the hosted fleet.
type FleetSnapshot struct {
	WindowDays int                    `json:"window_days"`
	Merchants  FleetMerchantFunnel    `json:"merchants"`
	Revenue    []FleetCurrencyRevenue `json:"revenue"`
	Rails      []FleetRailHealth      `json:"rails"`
	MRR        []FleetMRR             `json:"mrr"`
}

// FleetWeeklyPoint is one week's fleet movement; quiet weeks are zero-filled.
type FleetWeeklyPoint struct {
	WeekStart             time.Time `json:"week_start"`
	NewMerchants          int64     `json:"new_merchants"`
	ActiveMerchants       int64     `json:"active_merchants"`
	CanceledSubscriptions int64     `json:"canceled_subscriptions"`
}

// FleetWeeklyVolume is one week's settled sale volume in one currency.
type FleetWeeklyVolume struct {
	WeekStart     time.Time `json:"week_start"`
	Currency      string    `json:"currency"`
	Payments      int64     `json:"payments"`
	SettledAmount int64     `json:"settled_amount,string"`
}

// FleetSeries is the windowed weekly trend series of the hosted fleet.
type FleetSeries struct {
	Weeks  int                 `json:"weeks"`
	Points []FleetWeeklyPoint  `json:"points"`
	Volume []FleetWeeklyVolume `json:"volume"`
}

// MerchantRetirementCursor is the keyset position after a candidate.
type MerchantRetirementCursor struct {
	CreatedAt  time.Time
	MerchantID MerchantID
}

// MerchantRetirementCandidatesRequest pages live, group-bound, unreserved
// merchants created before CreatedBefore, oldest first.
type MerchantRetirementCandidatesRequest struct {
	CreatedBefore time.Time
	After         *MerchantRetirementCursor
	// Limit is the page size, 1..500.
	Limit int
}

// MerchantRetirementCandidate is one merchant and its activity fact.
type MerchantRetirementCandidate struct {
	MerchantID MerchantID
	Slug       string
	GroupID    string
	CreatedAt  time.Time
	// Used reports any retirement-blocking activity.
	Used bool
}

// MerchantRetirementCandidatePage is one keyset page; Next is nil at the end.
type MerchantRetirementCandidatePage struct {
	Candidates []MerchantRetirementCandidate
	Next       *MerchantRetirementCursor
}

// MerchantRetirementRefusal names why a merchant was not retired.
type MerchantRetirementRefusal string

const (
	MerchantRetirementRefusedNotLive       MerchantRetirementRefusal = "not_live"
	MerchantRetirementRefusedGroupMismatch MerchantRetirementRefusal = "group_mismatch"
	MerchantRetirementRefusedReserved      MerchantRetirementRefusal = "reserved"
	MerchantRetirementRefusedActive        MerchantRetirementRefusal = "active"
)

// MerchantRetirement reports one retirement attempt: Retired once the
// tombstone is committed, Refusal otherwise.
type MerchantRetirement struct {
	Retired bool
	Refusal MerchantRetirementRefusal
}

// The standalone server's merchant account routes: the signed-in user's
// merchants, the merchant's name, API keys and team.

// CreateMerchantRequest creates a merchant the signed-in user owns.
type CreateMerchantRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// MerchantName is a merchant's name, as a rename leaves it.
type MerchantName struct {
	ID   MerchantID `json:"id"`
	Name string     `json:"name"`
}

// RenameMerchantRequest renames the merchant; the former name keeps
// forwarding to it under the deployment's naming policy.
type RenameMerchantRequest struct {
	Name string `json:"name"`
}

// APIKey is one of the merchant's API keys, without its secret.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// CreateAPIKeyRequest mints a key holding one of the merchant roles (viewer,
// support, owner).
type CreateAPIKeyRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// CreatedAPIKey is a new key with its secret, shown this once.
type CreatedAPIKey struct {
	APIKey
	Secret string `json:"secret"`
}

// TeamMember is a user holding a merchant role.
type TeamMember struct {
	UserID   string  `json:"user_id"`
	Email    *string `json:"email"`
	Username *string `json:"username"`
	Role     string  `json:"role"`
}

// TeamInvite is a single-use link that registers its holder and adds them to
// the team. Its URL is answered only when it is made.
type TeamInvite struct {
	ID         string     `json:"id"`
	Role       string     `json:"role"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RedeemedAt *time.Time `json:"redeemed_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// InviteTeamMemberRequest invites an email address with a role.
type InviteTeamMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// TeamInviteResult is an invitation's outcome: Member when an account that
// verified the address was added at once, else Invite and its URL to share.
type TeamInviteResult struct {
	Member *TeamMember `json:"member"`
	Invite *TeamInvite `json:"invite"`
	URL    *string     `json:"url"`
}

// SetTeamRoleRequest changes a member's role.
type SetTeamRoleRequest struct {
	Role string `json:"role"`
}
