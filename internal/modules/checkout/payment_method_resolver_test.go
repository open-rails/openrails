package checkout

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
)

func TestPaymentMethodMatchesTargetPSP(t *testing.T) {
	targetPSPID := uuid.New()
	target := railTarget{Scope: &merchants.PSPScope{ID: targetPSPID}}

	tests := []struct {
		name    string
		method  *models.PaymentMethod
		target  railTarget
		wantErr string
	}{
		{name: "exact provider", method: &models.PaymentMethod{PspID: targetPSPID}, target: target},
		{name: "different provider", method: &models.PaymentMethod{PspID: uuid.New()}, target: target, wantErr: "different payment provider account"},
		{name: "missing method provider", method: &models.PaymentMethod{}, target: target, wantErr: "different payment provider account"},
		{name: "missing method", method: nil, target: target, wantErr: "method identity is unavailable"},
		{name: "unresolved target provider", method: &models.PaymentMethod{PspID: targetPSPID}, target: railTarget{}, wantErr: "identity is unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := paymentMethodMatchesTargetPSP(tt.method, tt.target)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("paymentMethodMatchesTargetPSP() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("paymentMethodMatchesTargetPSP() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
