package credits

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
)

func TestValidateCreditGrantSpec(t *testing.T) {
	days := 30
	tests := []struct {
		name    string
		ctype   string
		spec    models.CreditGrantSpec
		wantErr bool
	}{
		{"ok once default cadence", "api_credits", models.CreditGrantSpec{Amount: 1}, false},
		{"ok per_renewal", "api_credits", models.CreditGrantSpec{Amount: 1, Cadence: models.CreditGrantCadencePerRenewal}, false},
		{"ok expires", "api_credits", models.CreditGrantSpec{Amount: 1, ExpiresDays: &days}, false},
		{"bad empty type", "", models.CreditGrantSpec{Amount: 1}, true},
		{"bad amount", "api_credits", models.CreditGrantSpec{Amount: 0}, true},
		{"bad expires", "api_credits", models.CreditGrantSpec{Amount: 1, ExpiresDays: ptrInt(-1)}, true},
		{"bad cadence", "api_credits", models.CreditGrantSpec{Amount: 1, Cadence: "monthly"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCreditGrantSpec(tt.ctype, tt.spec)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func ptrInt(v int) *int { return &v }
