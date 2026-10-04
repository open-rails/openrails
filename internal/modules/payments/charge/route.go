package charge

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// ErrNoRoute: no single live PSP of the card's rail reaches the custodian
// holding it.
var ErrNoRoute = errors.New("no single PSP can charge this card")

// RoutePSP picks the PSP a charge on method goes through when no obligation
// names one: the PSP holding a PSP-held card, else the one live PSP of its
// rail that reaches its custodian.
func RoutePSP(ctx context.Context, q *gen.Queries, method gen.BillingPaymentMethod) (uuid.UUID, error) {
	if method.Custodian == models.CustodianPSP {
		if method.PspID == nil {
			return uuid.Nil, ErrNoRoute
		}
		return *method.PspID, nil
	}
	if method.CustodianID == nil {
		return uuid.Nil, ErrNoRoute
	}
	psps, err := q.ListCustodianRoutePSPs(ctx, gen.ListCustodianRoutePSPsParams{MerchantID: method.MerchantID, Rail: method.Rail, CustodianID: *method.CustodianID})
	if err != nil {
		return uuid.Nil, err
	}
	if len(psps) != 1 {
		return uuid.Nil, ErrNoRoute
	}
	return psps[0], nil
}
