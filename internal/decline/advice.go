package decline

import (
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// The networks' rules on charging a declined card again. Where a source is
// ambiguous, the card is not charged again.
//
// Visa: no reattempt after a category 1 code ("issuer will never approve":
// 04 07 12 14 15 41 43 46 57 R0 R1 R3); categories 2-4 allow a bounded number
// in 30 days. Mastercard: card-not-present hard declines (04 14 15 41 43 54)
// are never retried on the same card; Merchant Advice Code 03 is "do not try
// again", 21 a payment cancellation, 01 new account information, 40 and 41
// cards that won't recur (non-reloadable prepaid, single-use virtual), and 24
// to 30 the earliest retry after insufficient funds. Stripe: advice_code
// do_not_try_again, confirm_card_data or try_again_later.

// noReattempt is the union of Visa category 1 and Mastercard's hard declines,
// applied to every brand.
var noReattempt = map[string]bool{
	"04": true, "07": true, "12": true, "14": true, "15": true, "41": true, "43": true,
	"46": true, "54": true, "57": true, "R0": true, "R1": true, "R3": true,
}

// IssuerForbidsReattempt reports an issuer response code (NMI
// processor_response_code, Stripe network_decline_code) after which the card
// is never charged again.
func IssuerForbidsReattempt(code string) bool {
	return noReattempt[strings.ToUpper(strings.TrimSpace(code))]
}

// forbidsReattempt: Stripe's do_not_try_again (advice or decline code), MAC
// 03 or 21, or a forbidding issuer code.
func forbidsReattempt(e Evidence) bool {
	switch strings.TrimSpace(e.NetworkAdvice) {
	case "03", "21":
		return true
	}
	return strings.EqualFold(strings.TrimSpace(e.Advice), "do_not_try_again") || IssuerForbidsReattempt(e.IssuerCode) ||
		strings.EqualFold(e.Rail, "stripe") && strings.EqualFold(strings.TrimSpace(e.Code), "do_not_try_again")
}

// needsNewData: the card's data must change first (Stripe's
// confirm_card_data, MAC 01).
func needsNewData(e Evidence) bool {
	return strings.EqualFold(strings.TrimSpace(e.Advice), "confirm_card_data") || strings.TrimSpace(e.NetworkAdvice) == "01"
}

// cardWontRecur: Mastercard says the card is a non-reloadable prepaid card
// (MAC 40) or a single-use virtual card (41); only another card can pay.
func cardWontRecur(e Evidence) bool {
	advice := strings.TrimSpace(e.NetworkAdvice)
	return advice == "40" || advice == "41"
}

// retryAfter is Mastercard's earliest retry after an insufficient-funds
// decline (MAC 24 to 30).
var retryAfter = map[string]time.Duration{
	"24": time.Hour, "25": 24 * time.Hour, "26": 2 * 24 * time.Hour, "27": 4 * 24 * time.Hour,
	"28": 6 * 24 * time.Hour, "29": 8 * 24 * time.Hour, "30": 10 * 24 * time.Hour,
}

// advised applies the networks' advice: new card data turns an unspecific
// reason into update_cardholder_data, a forbidden reattempt or a card that
// won't recur waits for a new card, and MAC 24 to 30 delay the next retry.
// Advice never makes a refusal terminal; a rail code that already stops
// charging keeps its action.
func (r Result) advised(e Evidence) Result {
	r.NoReattempt = forbidsReattempt(e)
	r.RetryAfter = retryAfter[strings.TrimSpace(e.NetworkAdvice)]
	if needsNewData(e) && unspecific(r.Reason) {
		r = r.with(billing.DeclineUpdateCardholderData)
	}
	if r.Action.StopsCharging() {
		return r
	}
	switch {
	case r.NoReattempt || cardWontRecur(e):
		r.Action, r.Category, r.Transient = FixPaymentMethod, IssuerHard, false
	case needsNewData(e):
		r.Action, r.Category, r.Transient = FixPaymentMethod, CardData, false
	}
	return r
}

// HolderCanClear reports a fix_payment_method reason the cardholder can clear
// on the same card: by authenticating, or by correcting its billing address.
func HolderCanClear(reason billing.DeclineReason) bool {
	switch reason {
	case billing.DeclineAuthenticationRequired, billing.DeclineIncorrectZip, billing.DeclineIncorrectAddress:
		return true
	}
	return false
}
