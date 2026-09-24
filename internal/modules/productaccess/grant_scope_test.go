package productaccess

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A caller cannot choose the merchant a grant lands under; refusal precedes persistence (nil DB).
func TestGrantInsertRefusesMissingOrForeignMerchant(t *testing.T) {
	repo := NewProductAccessGrantRepo(nil)
	foreign := uuid.New()
	g := &models.ProductAccessGrant{MerchantID: foreign}
	require.ErrorIs(t, repo.Insert(context.Background(), g), merchant.ErrNoMerchant)
	require.ErrorContains(t, repo.Insert(merchant.WithID(context.Background(), merchant.ID(uuid.New())), g), "does not match")
	require.Equal(t, foreign, g.MerchantID, "a refused grant must not be rebound and retried silently")
}
