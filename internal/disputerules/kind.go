// Package disputerules classifies card disputes and fraud reports and holds the
// card networks' monitoring programs. It reads nothing, so every rail, the
// reconcile pull and the metrics classify alike.
package disputerules

import (
	"strings"

	"github.com/open-rails/openrails/billing"
)

// StripeKind classifies a Stripe dispute by its status, then its card
// case_type. An inquiry the issuer escalated keeps no warning_ status, so the
// status wins over a case_type of inquiry.
func StripeKind(status, caseType string) billing.DisputeKind {
	status = strings.ToLower(strings.TrimSpace(status))
	caseType = strings.ToLower(strings.TrimSpace(caseType))
	switch {
	case status == "prevented", caseType == "resolution", caseType == "block":
		return billing.DisputePrevented
	case strings.HasPrefix(status, "warning_"):
		return billing.DisputeInquiry
	case status == "needs_response", status == "under_review", status == "won", status == "lost",
		strings.HasPrefix(status, "appeal_"):
		return billing.DisputeChargeback
	case caseType == "inquiry":
		return billing.DisputeInquiry
	}
	return billing.DisputeChargeback
}

// StripePreventionRefunds reports whether a prevented Stripe dispute returned
// the money: an RDR resolution does, a CE 3.0 block does not.
func StripePreventionRefunds(caseType string) bool {
	return strings.EqualFold(strings.TrimSpace(caseType), "resolution")
}

// StripeStatus is a Stripe dispute's status in OpenRails' terms.
func StripeStatus(status string) billing.DisputeStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "won":
		return billing.DisputeWon
	case "lost":
		return billing.DisputeLost
	case "warning_closed", "prevented":
		return billing.DisputeClosed
	}
	return billing.DisputeOpen
}

// Network is the card network a brand as a provider spells it belongs to;
// billing.card_network in the schema is the same table.
func Network(brand string) billing.CardNetwork {
	var b strings.Builder
	for _, r := range strings.ToLower(brand) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	switch b.String() {
	case "", "unknown":
		return billing.NetworkUnknown
	case "visa":
		return billing.NetworkVisa
	case "mastercard", "mc":
		return billing.NetworkMastercard
	case "amex", "americanexpress":
		return billing.NetworkAmex
	case "discover", "disc":
		return billing.NetworkDiscover
	case "jcb":
		return billing.NetworkJCB
	case "diners", "dinersclub":
		return billing.NetworkDiners
	case "unionpay", "chinaunionpay":
		return billing.NetworkUnionPay
	}
	return billing.NetworkOther
}
