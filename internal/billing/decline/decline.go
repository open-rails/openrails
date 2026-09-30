// Package decline is the one table for payment refusals (#1109). Every rail
// code maps to one openrails.DeclineReason; this package fixes each reason's
// category and dunning action, and the public type what the buyer is told.
// Nothing else in OpenRails maps a decline code.
package decline

import (
	"context"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
)

// Category is what failed.
type Category string

const (
	// Approved is not a refusal; attempts use it for approvals.
	Approved Category = "approved"
	// CardData: the details don't match an open, valid account (typo, expired,
	// closed or reissued card).
	CardData Category = "card_data"
	// IssuerSoft: the issuer declined a well-formed request that may pass later.
	IssuerSoft Category = "issuer_soft"
	// IssuerHard: the issuer will not approve this card for this purchase.
	IssuerHard Category = "issuer_hard"
	// GatewayRule: the PSP's own rule refused it; the issuer never saw it.
	GatewayRule Category = "gateway_rule"
	// SystemError: gateway, processor, configuration or comms failure.
	SystemError Category = "system_error"
	Unknown     Category = "unknown"
)

// Action is the dunning decision (or#870). The zero value retries: missing
// evidence never costs a customer their subscription.
type Action string

const (
	// Retry keeps the dunning schedule.
	Retry Action = "retry"
	// FixPaymentMethod stops charging this card and waits for a new one; access
	// and the stored card are untouched, and a replaced card resumes dunning.
	FixPaymentMethod Action = "fix_payment_method"
	// NonRecoverable cancels at the rail. The stored payment method is never
	// touched: only its owner deletes it.
	NonRecoverable Action = "non_recoverable"
)

func (a Action) String() string {
	if a == "" {
		return string(Retry)
	}
	return string(a)
}

// StopsCharging reports whether charges against the card stop.
func (a Action) StopsCharging() bool { return a == FixPaymentMethod || a == NonRecoverable }

// Coverage says how much the table knew about a code.
type Coverage string

const (
	// Mapped: the rail's table has the code.
	Mapped Coverage = "mapped"
	// Unmapped: the rail has a table and the code is not in it. Retried by
	// doctrine, and alert-worthy.
	Unmapped Coverage = "unmapped"
	// NoCode: nothing to map (no code recorded, or a rail without a table).
	NoCode Coverage = "no_code"
)

// Result is the classifier's answer for one refusal.
type Result struct {
	Rail string
	// Code is the rail code in its canonical form (NMI: the numeric
	// response_code).
	Code      string
	Reason    openrails.DeclineReason
	Category  Category
	Action    Action
	Transient bool
	Coverage  Coverage
}

// NeedsMapping reports a code the rail's table does not know.
func (r Result) NeedsMapping() bool { return r.Coverage == Unmapped }

// Evidence is everything a rail returned about one authorization: its answer
// and the card it used.
type Evidence struct {
	Rail string
	// Code is the rail's code: NMI response_code (or its localization id),
	// Stripe decline_code, CCBill BE-nnn.
	Code string
	// FallbackCode is Stripe's error code when Code is its decline_code.
	FallbackCode string
	// AVS and CVV are NMI's avsresponse/cvvresponse letters, or Stripe's
	// address_postal_code_check/cvc_check.
	AVS, CVV string
	// Text is the gateway's response text.
	Text string
	// IssuerCode and IssuerText are the issuer's raw answer (NMI
	// processor_response_code/text).
	IssuerCode, IssuerText string
	// The card the PSP reports: BIN, brand, last four, and whether a network
	// token stood in for the card number.
	CardBIN, CardBrand, CardLast4 string
	NetworkToken                  bool
}

// Classify answers what a rail code means.
func Classify(rail, code string) Result {
	return ClassifyEvidence(Evidence{Rail: rail, Code: code})
}

// ReasonFor is Classify's reason as recorded in failure_reason columns.
func ReasonFor(rail, code string) string {
	return string(Classify(rail, code).Reason)
}

// ClassifyEvidence classifies a refusal with its AVS/CVV results: a security
// code mismatch is the reason whatever the code said, and an address mismatch
// explains a generic decline.
func ClassifyEvidence(e Evidence) Result {
	r := lookup(e.Rail, e.Code, e.Text)
	if r.Coverage != Mapped && strings.TrimSpace(e.FallbackCode) != "" {
		if fb := lookup(e.Rail, e.FallbackCode, e.Text); fb.Coverage == Mapped {
			r = fb
		}
	}
	if r.Reason.FraudSignal() {
		return r
	}
	generic := r.Reason == openrails.DeclineGeneric || r.Reason == openrails.DeclineDoNotHonor || r.Reason == openrails.DeclineIncorrectNumber || r.Coverage != Mapped
	switch {
	case cvvMismatch(r.Rail, e.CVV):
		r = r.with(openrails.DeclineIncorrectCVC)
	case generic && zipMismatch(r.Rail, e.AVS):
		r = r.with(openrails.DeclineIncorrectZip)
	case generic && addressMismatch(r.Rail, e.AVS):
		r = r.with(openrails.DeclineIncorrectAddress)
	}
	return r
}

func (r Result) with(reason openrails.DeclineReason) Result {
	spec := reasons[reason]
	r.Reason, r.Category, r.Action, r.Transient = reason, spec.category, spec.action, spec.transient
	return r
}

func lookup(rail, code, text string) Result {
	r := Result{Rail: strings.ToLower(strings.TrimSpace(rail)), Code: strings.TrimSpace(code)}
	table, ok := rails[r.Rail]
	if !ok || r.Code == "" {
		r.Coverage = NoCode
		return r.with(openrails.DeclineUnknown)
	}
	r.Code = table.canonical(r.Code)
	reason, found := table.codes[r.Code]
	if !found {
		r.Coverage = Unmapped
		return r.with(openrails.DeclineUnknown)
	}
	if r.Rail == "nmi" && r.Code == "300" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), "duplicate transaction") {
		reason = openrails.DeclineDuplicateTransaction
	}
	r.Coverage = Mapped
	return r.with(reason)
}

// AlertUnmapped logs the one loud line for a code no table knows. An unmapped
// code is retried like an ordinary decline, so nothing downstream looks wrong;
// this line is the only place the gap is visible.
func AlertUnmapped(ctx context.Context, r Result) {
	if !r.NeedsMapping() {
		return
	}
	log.WithContext(ctx).WithFields(log.Fields{"rail": r.Rail, "failure_code": r.Code}).
		Error("UNMAPPED decline code: retried by doctrine. Map it in internal/billing/decline")
}

// NMIResponseCode is the numeric NMI code of any recorded form (225,
// nmi_response_225, invalid_card_security_code, nmi_invalid_card_security_code),
// or 0.
func NMIResponseCode(code string) int {
	n, _ := strconv.Atoi(rails["nmi"].canonical(code))
	return n
}
