package productaccess

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestGrantInsertCannotChooseItsMerchant(t *testing.T) {
	repo := NewProductAccessGrantRepo(nil)
	foreign := uuid.New()
	grant := &models.ProductAccessGrant{MerchantID: foreign}
	require.Error(t, repo.Insert(context.Background(), grant), "missing scope must fail before database access")
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	require.ErrorContains(t, repo.Insert(ctx, grant), "does not match")
	require.Equal(t, foreign, grant.MerchantID, "a refused grant must not be rebound and retried silently")
}
