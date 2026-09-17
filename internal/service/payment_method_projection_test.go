package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestPaymentMethodFromModelCarriesPSPProvenance(t *testing.T) {
	pspID := uuid.New()
	method := paymentMethodFromModel(&models.PaymentMethod{
		ID:    uuid.New(),
		PspID: pspID,
		Rail:  models.RailNMI,
	})
	if method.PSPID != pspID.String() {
		t.Fatalf("PSPID = %q, want %q", method.PSPID, pspID)
	}
}

func TestPaymentMethodFromModelDoesNotExposeNilUUIDSentinel(t *testing.T) {
	method := paymentMethodFromModel(&models.PaymentMethod{ID: uuid.New(), Rail: models.RailNMI})
	if method.PSPID != "" {
		t.Fatalf("PSPID = %q, want empty identity", method.PSPID)
	}
}
