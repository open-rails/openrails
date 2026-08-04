package collection

import "testing"

// TestNMIDeclineBuckets is the or#870 code→bucket table, asserted code by code.
// It is deliberately EXHAUSTIVE over every NMI response code the gateway
// publishes: the whole point of the issue is that no code may be left with an
// undefined answer, so a code missing from this test is a code nobody decided.
func TestNMIDeclineBuckets(t *testing.T) {
	// Bucket 1 — ours or transient. Keep the dunning schedule.
	retry := []int{
		100, // approved (not a decline at all)
		200, // generic decline
		202, // insufficient funds
		203, // over limit
		260, // declined with further instructions
		264, // retry in a few days
		300, // rejected by gateway
		400, // processor error
		410, // invalid merchant configuration  — OURS
		411, // merchant account inactive       — OURS
		420, // communication error
		421, // communication error with issuer
		430, // duplicate transaction
		440, // format error
		441, // invalid transaction information
		460, // feature not available
	}
	// Bucket 2 — their card, fixable. Stop charging, keep access, ask them.
	// 250/251 (pick-up/lost) sit here by owner decision (or#870, 2026-07-29):
	// same treatment as an expired card. The instrument is dead but the customer
	// did nothing wrong, and a reissued card works — losing a wallet must not
	// cost a subscription.
	fix := []int{201, 204, 220, 221, 222, 223, 224, 225, 226, 240, 250, 251, 263, 461}
	// Bucket 3 — non-recoverable. Cancel at the rail, never touch the card.
	// 252/253 carry a fraud signal; 261/262 are the issuer withdrawing the mandate.
	nonRecoverable := []int{252, 253, 261, 262}

	for _, code := range retry {
		if got := ClassifyNMIResponseCode(code); got != DeclineRetry {
			t.Errorf("NMI %d: got %v, want retry", code, got)
		}
	}
	for _, code := range fix {
		if got := ClassifyNMIResponseCode(code); got != DeclineFixPaymentMethod {
			t.Errorf("NMI %d: got %v, want fix_payment_method", code, got)
		}
	}
	for _, code := range nonRecoverable {
		if got := ClassifyNMIResponseCode(code); got != DeclineNonRecoverable {
			t.Errorf("NMI %d: got %v, want non_recoverable", code, got)
		}
	}

	// Every published code has exactly one answer and appears exactly once.
	seen := map[int]bool{}
	for _, set := range [][]int{retry, fix, nonRecoverable} {
		for _, code := range set {
			if seen[code] {
				t.Errorf("NMI %d classified in two buckets", code)
			}
			seen[code] = true
		}
	}
	for code := range nmiDeclineOutcomes {
		if !seen[code] {
			t.Errorf("NMI %d is in the classifier table but nobody asserted its bucket", code)
		}
	}
}

// TestUnknownCodesAreAlwaysRetry is the doctrine's load-bearing safety
// property: missing evidence must never cost a customer their subscription.
// An unrecognized code can land in bucket 1 and nowhere else.
func TestUnknownCodesAreAlwaysRetry(t *testing.T) {
	cases := []struct{ rail, code string }{
		{"nmi", ""},
		{"nmi", "999"}, // not in NMI's published set
		{"nmi", "0"},   // no code recorded
		{"nmi", "wat"}, // not even numeric
		{"nmi", "brand_new_localization_id"},
		{"stripe", "some_code_stripe_added_last_tuesday"},
		{"stripe", ""},
		{"ccbill", "261"}, // NMI's number on a rail that does not speak it
		{"ccbill", "anything"},
		{"solana", "anything"},
		{"a_rail_that_does_not_exist", "252"},
	}
	for _, c := range cases {
		if got := ClassifyDecline(c.rail, c.code); got != DeclineRetry {
			t.Errorf("ClassifyDecline(%q, %q) = %v, want retry — unknown codes must never stop charging or cancel", c.rail, c.code, got)
		}
	}
	if got := ClassifyNMIResponseCode(0); got != DeclineRetry {
		t.Errorf("ClassifyNMIResponseCode(0) = %v, want retry", got)
	}
}

// TestNMILocalizationIDsAgreeWithNumericCodes: the payments mirror records
// whichever form the rail returned, so both must give the same answer.
func TestNMILocalizationIDsAgreeWithNumericCodes(t *testing.T) {
	for code, want := range nmiDeclineOutcomes {
		id := nmiDeclineLocalizationIDs[code]
		if id == "" {
			t.Errorf("NMI %d has a bucket but no localization id; a decline recorded in string form would silently fall to retry", code)
			continue
		}
		if got := ClassifyDecline("nmi", id); got != want {
			t.Errorf("ClassifyDecline(nmi, %q) = %v, want %v (numeric %d)", id, got, want, code)
		}
		// nmidirect records some codes with an nmi_ prefix.
		if got := ClassifyDecline("mobius", "nmi_"+id); got != want {
			t.Errorf("ClassifyDecline(mobius, nmi_%s) = %v, want %v", id, got, want)
		}
	}
}

// TestStripeAndCCBillDeclineBuckets maps the other rails onto the same three
// buckets. Stripe's bucket 3 is EXACTLY the pre-or#870 non-retryable set: this
// doctrine reorganizes the answers, it must not widen who gets cancelled.
func TestStripeAndCCBillDeclineBuckets(t *testing.T) {
	cases := []struct {
		rail, code string
		want       DeclineOutcome
	}{
		{"stripe", "insufficient_funds", DeclineRetry},
		{"stripe", "generic_decline", DeclineRetry},
		{"stripe", "card_declined", DeclineRetry},
		{"stripe", "processing_error", DeclineRetry},
		{"stripe", "try_again_later", DeclineRetry},
		{"stripe", "issuer_not_available", DeclineRetry},
		{"stripe", "duplicate_transaction", DeclineRetry},
		{"stripe", "card_velocity_exceeded", DeclineRetry},
		{"stripe", "withdrawal_count_limit_exceeded", DeclineRetry},

		{"stripe", "expired_card", DeclineFixPaymentMethod},
		{"stripe", "incorrect_cvc", DeclineFixPaymentMethod},
		{"stripe", "invalid_cvc", DeclineFixPaymentMethod},
		{"stripe", "incorrect_number", DeclineFixPaymentMethod},
		{"stripe", "do_not_honor", DeclineFixPaymentMethod},
		{"stripe", "call_issuer", DeclineFixPaymentMethod},
		{"stripe", "transaction_not_allowed", DeclineFixPaymentMethod},
		{"stripe", "card_not_supported", DeclineFixPaymentMethod},
		{"stripe", "authentication_required", DeclineFixPaymentMethod},

		{"stripe", "revocation_of_authorization", DeclineNonRecoverable},
		{"stripe", "revocation_of_all_authorizations", DeclineNonRecoverable},
		{"stripe", "stop_payment_order", DeclineNonRecoverable},
		{"stripe", "lost_card", DeclineNonRecoverable},
		{"stripe", "stolen_card", DeclineNonRecoverable},
		{"stripe", "pickup_card", DeclineNonRecoverable},
		{"stripe", "fraudulent", DeclineNonRecoverable},
		{"stripe", "invalid_account", DeclineNonRecoverable},
		{"stripe", "no_account", DeclineNonRecoverable},

		// CCBill publishes no enumerated decline vocabulary, so every code is
		// bucket 1 — OpenRails never cancels a CCBill customer on a decline
		// string it cannot read.
		{"ccbill", "insufficient_funds", DeclineRetry},
		{"ccbill", "stolen_card", DeclineRetry},
		// Solana's crank vocabulary carries no issuer mandate signal.
		{"solana", "declined_stop_all_recurring_payments", DeclineRetry},
	}
	for _, c := range cases {
		if got := ClassifyDecline(c.rail, c.code); got != c.want {
			t.Errorf("ClassifyDecline(%q, %q) = %v, want %v", c.rail, c.code, got, c.want)
		}
	}
}

// TestDeclineRetryIsTheZeroValue: every FailMembership caller that does not set
// Decline gets bucket 1 by construction, not by remembering to.
func TestDeclineRetryIsTheZeroValue(t *testing.T) {
	var zero DeclineOutcome
	if zero != DeclineRetry {
		t.Fatalf("zero DeclineOutcome = %v, want retry", zero)
	}
	if DeclineRetry.StopsCharging() {
		t.Error("bucket 1 must keep charging on the schedule")
	}
	if !DeclineFixPaymentMethod.StopsCharging() || !DeclineNonRecoverable.StopsCharging() {
		t.Error("buckets 2 and 3 must both stop charging")
	}
}

// TestCustodianProxiedDeclinesClassifyThroughNMI (or#879) pins the property
// that survived deleting the `vaulted_card` alias branch: a Basis-Theory-held
// card is charged at an NMI gateway and returns NMI's classic response, so it
// must classify through the NMI taxonomy — same vocabulary, different
// transport. Before or#879 this worked only because someone remembered to
// write `case "nmi", "mobius", "vaulted_card":`; now it works because the rail
// IS nmi and there is no third value to forget.
func TestCustodianProxiedDeclinesClassifyThroughNMI(t *testing.T) {
	// One representative of each bucket, in both code shapes the charge path
	// records (verbatim numeric, and the #733 localization form).
	cases := []struct {
		code string
		want DeclineOutcome
	}{
		{"202", DeclineRetry},                         // insufficient funds
		{"nmi_response_202", DeclineRetry},            // same evidence, other shape
		{"223", DeclineFixPaymentMethod},              // expired card
		{"nmi_expired_card", DeclineFixPaymentMethod}, // localization-id shape
		{"262", DeclineNonRecoverable},                // stop this recurring program
	}
	for _, c := range cases {
		// The rail carried by a proxied charge is plain nmi.
		if got := ClassifyDecline("nmi", c.code); got != c.want {
			t.Errorf("ClassifyDecline(nmi, %q) = %q, want %q", c.code, got, c.want)
		}
		// The PSP-key spelling the row vocabulary also uses.
		if got := ClassifyDecline("mobius", c.code); got != c.want {
			t.Errorf("ClassifyDecline(mobius, %q) = %q, want %q", c.code, got, c.want)
		}
		// And the retired value classifies as nothing anyone decided — it must
		// never reappear as a rail.
		if got := ClassifyDecline("vaulted_card", c.code); got != DeclineRetry {
			t.Errorf("ClassifyDecline(vaulted_card, %q) = %q; the value is retired (or#879) and must not carry a taxonomy", c.code, got)
		}
	}
}
