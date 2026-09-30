package payments

import (
	"strings"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// Attempt kinds stamped on payments at write time (#733).
const (
	AttemptInitial = "initial"
	AttemptRenewal = "renewal"
)

// DefaultTokenType is the #796 token_type stamp for charge records created OFF
// the seam (provider-driven renewals, webhook/converge folds), where no
// charge.Result carries the rail's answer. It needs BOTH axes (or#879): the
// rail says a card was charged at all, the CUSTODIAN says which credential form
// reached the network — the PSP's own stored token, or a detokenized FPAN
// proxied in from a third party (NT charging is config-gated off on NMI
// gateways). "" = not a stamped card rail.
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
