// Package disputes classifies card disputes and fraud reports and holds the
// card networks' monitoring programs. It has no dependencies, so every rail,
// the reconcile pull and the metrics read it alike.
package disputes

import "strings"

// Kind is what a provider case is. Only a chargeback moves the merchant's
// money back to the cardholder and counts in a network's dispute ratio.
type Kind string

const (
	// KindInquiry is the issuer asking before it disputes; nothing is
	// withdrawn and it counts nowhere.
	KindInquiry    Kind = "inquiry"
	KindChargeback Kind = "chargeback"
	// KindPrevented is a dispute resolved before it was filed: an automatic
	// refund (Visa RDR) or a block (Compelling Evidence 3.0).
	KindPrevented Kind = "prevented"
)

// StripeKind classifies a Stripe dispute by its status, then its card
// case_type. An inquiry the issuer escalated keeps no warning_ status, so the
// status wins over a case_type of inquiry.
func StripeKind(status, caseType string) Kind {
	status = strings.ToLower(strings.TrimSpace(status))
	caseType = strings.ToLower(strings.TrimSpace(caseType))
	switch {
	case status == "prevented", caseType == "resolution", caseType == "block":
		return KindPrevented
	case strings.HasPrefix(status, "warning_"):
		return KindInquiry
	case status == "needs_response", status == "under_review", status == "won", status == "lost",
		strings.HasPrefix(status, "appeal_"):
		return KindChargeback
	case caseType == "inquiry":
		return KindInquiry
	}
	return KindChargeback
}

// StripePreventionRefunds reports whether a prevented Stripe dispute returned
// the money: an RDR resolution does, a CE 3.0 block does not.
func StripePreventionRefunds(caseType string) bool {
	return strings.EqualFold(strings.TrimSpace(caseType), "resolution")
}
