package billingauth

import (
	"context"
	"net/http"

	"github.com/open-rails/openrails/pkg/merchant"
)

// Gate protects merchant-scoped routes.
type Gate interface {
	Authorize(ctx context.Context, r *http.Request, permission string) (Principal, error)
}

// Principal is the caller identity resolved by a Gate.
type Principal struct {
	MerchantID  merchant.ID
	UserContext UserContext
	// Permissions is the credential's resolved grant set for NON-USER principals
	// (API keys, service JWTs, host/delegated principals) — consumers that need
	// no-escalation checks (#757 api-key minting) read it. Empty for user
	// sessions: those carry UserContext.UserID and are checked against live
	// AuthKit group state instead.
	Permissions []string
}

// GateError maps authorization failures to stable HTTP responses.
type GateError struct {
	Status  int
	Message string
}

func (e GateError) Error() string { return e.Message }
