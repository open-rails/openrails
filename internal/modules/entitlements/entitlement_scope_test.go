package entitlements

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A caller cannot choose the merchant an entitlement lands under, nor store an empty window; refusal precedes persistence (nil DB).
func TestInsertRefusesBeforePersistence(t *testing.T) {
	svc := NewEntitlementService(nil)
	foreign := uuid.New()
	e := &models.Entitlement{MerchantID: foreign}
	require.ErrorIs(t, svc.Insert(context.Background(), e), merchant.ErrNoMerchant)
	require.ErrorContains(t, svc.Insert(merchant.WithID(context.Background(), merchant.ID(uuid.New())), e), "merchant does not match context")
	require.Equal(t, foreign, e.MerchantID, "refusal must not rewrite the caller's model")

	now := time.Now()
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	for _, end := range []time.Time{now, now.Add(-time.Hour)} {
		require.ErrorContains(t, svc.Insert(ctx, &models.Entitlement{StartAt: now, EndAt: &end}), "must be after start_at")
	}
}
