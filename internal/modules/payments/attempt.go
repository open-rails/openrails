package payments

import (
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// Attempt kinds stamped on payments at write time.
const (
	AttemptInitial = "initial"
	AttemptRenewal = "renewal"
)

// DefaultTokenType is the token_type stamp for a charge recorded off the seam
// (provider renewals, webhook and converge folds), where no charge.Result
// carries the rail's answer. The rail says a card was charged; the custodian
// says which credential form reached the network: the PSP's stored token, or a
// third party's FPAN through its proxy (NMI gateways charge no network
// tokens). "" = not a stamped card rail.
func DefaultTokenType(rail, custodian string) string {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "stripe":
		if custodian == models.CustodianPSP {
			return charge.TokenTypePSPToken
		}
		return ""
	case "nmi":
		switch strings.TrimSpace(custodian) {
		case models.CustodianBasisTheory, models.CustodianHyperSwitch:
			return charge.TokenTypePANViaProxy
		case models.CustodianPSP:
			return charge.TokenTypePSPToken
		default:
			// Custody unstated: stamp nothing. A guessed form would skew the
			// token_type dimension of the decline metrics.
			return ""
		}
	default:
		return ""
	}
}
