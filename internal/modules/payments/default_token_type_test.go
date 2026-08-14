package payments

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// TestDefaultTokenTypeNeedsBothAxes (or#879): rail alone can no longer say
// which credential form went to the network — that was the job the
// `vaulted_card` rail value was doing badly. Custody says it, and an unstated
// custodian stamps NOTHING rather than fabricating "the PSP held it".
func TestDefaultTokenTypeNeedsBothAxes(t *testing.T) {
	cases := []struct {
		rail, custodian, want string
	}{
		{"nmi", models.CustodianPSP, charge.TokenTypePSPToken},
		{"nmi", models.CustodianBasisTheory, charge.TokenTypePANViaProxy},
		{"NMI", models.CustodianBasisTheory, charge.TokenTypePANViaProxy},
		// No custody fact stated: stamp nothing. A guessed token_type would
		// silently skew the approval_rate dimension it exists to measure.
		{"nmi", "", ""},
		// Non-card rails stamp nothing regardless.
		{"stripe", models.CustodianPSP, ""},
		{"ccbill", "", ""},
		{"solana", "", ""},
		// The retired rail value is not a rail.
		{"vaulted_card", models.CustodianBasisTheory, ""},
		// Mobius is a PSP key on NMI, not a rail.
		{"mobius", models.CustodianPSP, ""},
	}
	for _, c := range cases {
		if got := DefaultTokenType(c.rail, c.custodian); got != c.want {
			t.Errorf("DefaultTokenType(%q, %q) = %q, want %q", c.rail, c.custodian, got, c.want)
		}
	}
}

func TestNormalizeFailureReasonRejectsPSPKeyAsRail(t *testing.T) {
	if got := NormalizeFailureReason("mobius", "202"); got != FailureUnknown {
		t.Fatalf("NormalizeFailureReason(mobius, 202) = %q, want %q", got, FailureUnknown)
	}
}
