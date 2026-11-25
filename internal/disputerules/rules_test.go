package disputerules

import (
	"maps"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		network        billing.CardNetwork
		code, provider string
		want           billing.DisputeReason
	}{
		{billing.NetworkVisa, "10.4", "", billing.DisputeFraud},
		{billing.NetworkVisa, "13.2", "", billing.DisputeCanceledRecurring},
		{billing.NetworkVisa, "12.6.1", "", billing.DisputeDuplicate},
		{billing.NetworkVisa, "13.7", "", billing.DisputeCreditNotProcessed},
		{billing.NetworkMastercard, "4863", "", billing.DisputeUnrecognized},
		{billing.NetworkMastercard, "4853", "product_not_received", billing.DisputeNotReceived},
		{billing.NetworkMastercard, "4853", "", billing.DisputeNotAsDescribed},
		{billing.NetworkUnknown, "4837", "", billing.DisputeFraud},
		{billing.NetworkUnknown, "13.1", "", billing.DisputeNotReceived},
		{billing.NetworkVisa, "", "subscription_canceled", billing.DisputeCanceledRecurring},
		{billing.NetworkUnknown, "", "Cardholder does not recognize transaction", billing.DisputeUnrecognized},
		{billing.NetworkUnknown, "", "", billing.DisputeUnknownReason},
	}
	for _, c := range cases {
		if got := Classify(c.network, c.code, c.provider); got != c.want {
			t.Errorf("Classify(%s, %q, %q) = %s, want %s", c.network, c.code, c.provider, got, c.want)
		}
	}
}

func TestStripeKind(t *testing.T) {
	cases := []struct {
		status, caseType string
		want             billing.DisputeKind
	}{
		{"warning_needs_response", "inquiry", billing.DisputeInquiry},
		{"warning_closed", "", billing.DisputeInquiry},
		{"needs_response", "inquiry", billing.DisputeChargeback},
		{"lost", "chargeback", billing.DisputeChargeback},
		{"prevented", "", billing.DisputePrevented},
		{"lost", "resolution", billing.DisputePrevented},
		{"charge_refunded", "", billing.DisputeUnknownKind},
	}
	for _, c := range cases {
		if got := StripeKind(c.status, c.caseType); got != c.want {
			t.Errorf("StripeKind(%q, %q) = %s, want %s", c.status, c.caseType, got, c.want)
		}
	}
}

func TestNetworkMatchesTheSchema(t *testing.T) {
	for brand, want := range map[string]billing.CardNetwork{"Visa": billing.NetworkVisa, "MASTERCARD": billing.NetworkMastercard, "mc": billing.NetworkMastercard,
		"American Express": billing.NetworkAmex, "Diners Club": billing.NetworkDiners, "": billing.NetworkUnknown, "eftpos_au": billing.NetworkOther} {
		if got := Network(brand); got != want {
			t.Errorf("Network(%q) = %s, want %s", brand, got, want)
		}
	}
}

func program(id billing.DisputeProgram) Program {
	for _, p := range Programs {
		if p.ID == id {
			return p
		}
	}
	panic(id)
}

func TestVAMPLevelsFollowTheirEffectiveDates(t *testing.T) {
	vamp := program(billing.ProgramVisaVAMP)
	excessive := func(month time.Time) int64 {
		for _, l := range vamp.InForce(month) {
			if l.Level == billing.DisputeLevelExcessive {
				return l.RatioBPS
			}
		}
		return 0
	}
	if got := excessive(time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)); got != 220 {
		t.Fatalf("March 2026 excessive = %d bps, want 220", got)
	}
	if got := excessive(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)); got != 150 {
		t.Fatalf("April 2026 excessive = %d bps, want 150", got)
	}
	if got := excessive(time.Date(2025, time.May, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("VAMP is not in force before June 2025, got %d bps", got)
	}
	for _, p := range Programs {
		for _, l := range p.Levels {
			if l.Source == "" || l.RatioBPS <= 0 {
				t.Errorf("%s %s has no source or ratio", p.ID, l.Level)
			}
		}
	}
}

func TestEvaluateProjectsTheMonth(t *testing.T) {
	month := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	now := month.AddDate(0, 0, 15)
	days := map[time.Time]Activity{}
	for d := 0; d < 15; d++ {
		days[month.AddDate(0, 0, d)] = Activity{Sales: 1000}
	}
	// 1,000 VAMP items over 15,000 sales so far: 6.7%, 2,000 projected, but
	// short of the 1,500 counted that excessive needs.
	days[month.AddDate(0, 0, 3)] = Activity{Sales: 1000, Chargebacks: 600, FraudReports: 400}
	st := program(billing.ProgramVisaVAMP).Evaluate(WindowFor(month, now, days))
	if st.Level != billing.DisputeLevelWarning || st.ProjectedCount != 2000 {
		t.Fatalf("VAMP standing %+v, want warning with 2000 projected", st)
	}
	days[month.AddDate(0, 0, 4)] = Activity{Sales: 1000, Chargebacks: 600}
	if st := program(billing.ProgramVisaVAMP).Evaluate(WindowFor(month, now, days)); st.Level != billing.DisputeLevelExcessive {
		t.Fatalf("VAMP standing %+v, want excessive at 1,600 counted", st)
	}
	// Mastercard divides by the previous month's sales; 60 counted reach the
	// warning's 50, not excessive's 100.
	prior := map[time.Time]Activity{month.AddDate(0, -1, 0): {Sales: 1000}, month.AddDate(0, 0, 10): {Chargebacks: 60}}
	ecp := program(billing.ProgramMastercardECP).Evaluate(WindowFor(month, now, prior))
	if ecp.Denominator != 1000 || ecp.ProjectedCount != 120 || ecp.Level != billing.DisputeLevelWarning {
		t.Fatalf("ECP standing %+v", ecp)
	}
	busy := map[time.Time]Activity{month.AddDate(0, -1, 0): {Sales: 100_000}, month.AddDate(0, 0, 10): {Chargebacks: 60}}
	if got := program(billing.ProgramMastercardECP).Evaluate(WindowFor(month, now, busy)); got.Level != billing.DisputeLevelNone {
		t.Fatalf("after a busy month the same chargebacks are below every level: %+v", got)
	}
	// Early in a month the last 30 days stand in for it.
	early := map[time.Time]Activity{month.AddDate(0, 0, -10): {Sales: 100, Chargebacks: 6}}
	if got := program(billing.ProgramStripe).Evaluate(WindowFor(month, month.AddDate(0, 0, 2), early)); got.Count != 6 || got.Level != billing.DisputeLevelExcessive {
		t.Fatalf("early-month Stripe standing %+v", got)
	}
}

// Minimums are what was counted, never what the month is projected to reach:
// four chargebacks ten days into a month reach no level, however fast they
// came.
func TestMinimumsAreActualCounts(t *testing.T) {
	month := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	days := map[time.Time]Activity{month.AddDate(0, 0, 2): {Sales: 100, Chargebacks: 4}}
	w := WindowFor(month, month.AddDate(0, 0, 10), days)
	if w.Factor != 3 {
		t.Fatalf("factor %v, want 3", w.Factor)
	}
	for _, id := range []billing.DisputeProgram{billing.ProgramVisaVAMP, billing.ProgramStripe} {
		if st := program(id).Evaluate(w); st.Level != billing.DisputeLevelNone {
			t.Errorf("%s reached %s on 4 actual chargebacks (projected %d)", id, st.Level, st.ProjectedCount)
		}
	}
	days[month.AddDate(0, 0, 3)] = Activity{Chargebacks: 1}
	if st := program(billing.ProgramStripe).Evaluate(WindowFor(month, month.AddDate(0, 0, 10), days)); st.Level != billing.DisputeLevelExcessive {
		t.Errorf("Stripe at 5 actual chargebacks over 100 sales: %s", st.Level)
	}
}

// A program's ratio is drawn against the levels in force in each month:
// VAMP's excessive level fell from 2.2% to 1.5% on 1 April 2026.
func TestProgramLines(t *testing.T) {
	inForce := func(id billing.DisputeProgram, month time.Time) map[float64]string {
		out := map[float64]string{}
		for _, l := range ProgramLines(id) {
			if (l.From == nil || !month.Before(*l.From)) && (l.Until == nil || month.Before(*l.Until)) {
				out[l.Value] = l.Label
			}
		}
		return out
	}
	march, april := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)
	if got := inForce(billing.ProgramVisaVAMP, march); !maps.Equal(got, map[float64]string{0.005: "warning", 0.022: "excessive"}) {
		t.Errorf("VAMP lines in March 2026 %v", got)
	}
	if got := inForce(billing.ProgramVisaVAMP, april); !maps.Equal(got, map[float64]string{0.005: "warning", 0.015: "excessive"}) {
		t.Errorf("VAMP lines in April 2026 %v", got)
	}
	if got := inForce(billing.ProgramVisaVAMP, time.Date(2025, time.May, 1, 0, 0, 0, 0, time.UTC)); len(got) != 0 {
		t.Errorf("VAMP lines before VAMP %v", got)
	}
	if got := inForce(billing.ProgramMastercardECP, april); !maps.Equal(got, map[float64]string{0.01: "warning", 0.015: "excessive", 0.03: "high excessive"}) {
		t.Errorf("ECM lines %v", got)
	}
}

// A day's activity is so far from the day's first instant: at midnight the
// chargeback just filed counts.
func TestWindowCountsNowsDay(t *testing.T) {
	month := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{month.AddDate(0, 0, 2), month.AddDate(0, 0, 10)} {
		if w := WindowFor(month, now, map[time.Time]Activity{now: {Chargebacks: 1}}); w.SoFar.Chargebacks != 1 {
			t.Errorf("at %s the day's chargeback is not counted: %+v", now, w.SoFar)
		}
	}
}
