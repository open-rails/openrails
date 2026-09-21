package entitlements

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInsertRejectsMissingOrForeignMerchantBeforePersistence(t *testing.T) {
	service := NewEntitlementService(nil)
	a, b := merchant.ID(uuid.New()), uuid.New()
	window := &models.Entitlement{MerchantID: b}
	require.ErrorIs(t, service.Insert(context.Background(), window), merchant.ErrNoMerchant)
	err := service.Insert(merchant.WithID(context.Background(), a), window)
	require.ErrorContains(t, err, "merchant does not match context")
	require.Equal(t, b, window.MerchantID, "refusal must not rewrite the caller's model")
}
