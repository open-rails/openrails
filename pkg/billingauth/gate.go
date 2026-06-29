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
}

// GateError maps authorization failures to stable HTTP responses.
type GateError struct {
	Status  int
	Message string
}

func (e GateError) Error() string { return e.Message }
