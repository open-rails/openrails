package server

import (
	"errors"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// The errors the server's methods answer, for errors.Is. AuthKit's own
// (iam.ErrInsufficientAuthority, iam.ErrRoleAssignmentEscalation,
// iam.ErrSessionRevoked) pass through unchanged.
var (
	// ErrMerchantNotFound: no merchant has the id. It is also
	// billing.ErrNotFound.
	ErrMerchantNotFound = merchants.ErrMerchantNotFound
	// ErrInvalidMerchantName: the name breaks the merchant name rule.
	ErrInvalidMerchantName = merchants.ErrInvalidName
	// ErrRenamesDisabled: Config.Auth.Naming forbids renames.
	ErrRenamesDisabled = merchants.ErrRenamesDisabled

	// ErrInvalidAPIHost: not a bare lowercase domain name.
	ErrInvalidAPIHost = merchants.ErrInvalidAPIHost
	// ErrAPIHostReserved: the host serves the deployment itself.
	ErrAPIHostReserved = merchants.ErrAPIHostReserved
	// ErrAPIHostTaken: another merchant routes from the host.
	ErrAPIHostTaken = merchants.ErrAPIHostTaken
	// ErrAPIHostClaimMissing: the merchant has no open claim to verify.
	ErrAPIHostClaimMissing = merchants.ErrAPIHostClaimMissing
	// ErrAPIHostUnproven: the challenge record does not carry the claim's
	// token yet.
	ErrAPIHostUnproven = merchants.ErrAPIHostUnproven

	// ErrUnknownMerchantRole: not owner, support or viewer.
	ErrUnknownMerchantRole = controlplane.ErrUnknownMerchantRole
	// ErrRoleEscalation: a CredentialActor grants beyond its permissions.
	ErrRoleEscalation = errors.New("server: the role exceeds the actor's own permissions")
	// ErrLastOwner: the change would leave the merchant without an owner.
	ErrLastOwner = controlplane.ErrCannotRemoveLastOwner
	// ErrNotTeamMember: the user holds no role on the merchant.
	ErrNotTeamMember = controlplane.ErrNotATeamMember
	// ErrTeamInvitesDisabled: the email has no verified account, and the
	// deployment registers nobody by invitation.
	ErrTeamInvitesDisabled = controlplane.ErrTeamInvitesDisabled

	// ErrFederatedGrantInvalidEmail: the email is not a bare address.
	ErrFederatedGrantInvalidEmail = controlplane.ErrFederatedGrantInvalidEmail
	// ErrFederatedGrantExists: the email is invited already, or the user
	// holds a grant on the merchant.
	ErrFederatedGrantExists = controlplane.ErrFederatedGrantExists
	// ErrFederatedGrantNotFound: the merchant has no grant with the id.
	ErrFederatedGrantNotFound = controlplane.ErrFederatedGrantNotFound
	// ErrFederatedGrantUnverified: the token carries no verified email.
	ErrFederatedGrantUnverified = controlplane.ErrFederatedGrantUnverified
)
