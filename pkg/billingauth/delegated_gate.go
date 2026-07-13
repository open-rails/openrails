package billingauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/pkg/merchant"
)

// DelegatedGate is the standard host-side Gate over a DelegatedAuthenticator:
// authenticate the delegated token, enforce one permission (wildcard grants
// supported), and map the delegated principal onto the billing Principal.
//
// Extracted verbatim from the byte-identical copies doujins and hentai0 each
// carried (doujins #770 U15) so the two hosts of the shared merchant cannot
// drift on authorization semantics.
type DelegatedGate struct {
	authn DelegatedAuthenticator
}

// NewDelegatedGate builds the standard delegated Gate for a host.
func NewDelegatedGate(authn DelegatedAuthenticator) DelegatedGate {
	return DelegatedGate{authn: authn}
}

// Authorize implements Gate.
func (g DelegatedGate) Authorize(ctx context.Context, r *http.Request, permission string) (Principal, error) {
	if g.authn == nil {
		return Principal{}, GateError{Status: http.StatusInternalServerError, Message: "authorization unavailable"}
	}
	principal, err := g.authn.AuthenticateDelegated(ctx, r)
	if err != nil {
		return Principal{}, GateError{Status: http.StatusUnauthorized, Message: UnauthenticatedMessage(err)}
	}
	if !HasPermission(principal.Permissions, permission) {
		return Principal{}, GateError{Status: http.StatusForbidden, Message: "permission_required"}
	}
	mid, err := merchant.ParseID(principal.MerchantID)
	if err != nil {
		return Principal{}, GateError{Status: http.StatusUnauthorized, Message: "delegated_principal_invalid"}
	}
	return Principal{
		MerchantID: mid,
		UserContext: UserContext{
			UserID:        principal.SubjectID,
			Email:         principal.Email,
			EmailVerified: principal.EmailVerified,
			Username:      principal.Username,
			Merchant:      principal.MerchantSlug,
		},
	}, nil
}

// HasPermission reports whether the grant set covers permission: an exact
// match, the global "*" grant, or a prefix wildcard ("billing:*" covers
// "billing:read"). Grants are trimmed before comparison.
func HasPermission(grants []string, permission string) bool {
	for _, grant := range grants {
		grant = strings.TrimSpace(grant)
		if grant == permission || grant == "*" {
			return true
		}
		if strings.HasSuffix(grant, ":*") && strings.HasPrefix(permission, strings.TrimSuffix(grant, "*")) {
			return true
		}
	}
	return false
}
