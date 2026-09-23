//go:build integration

package merchants

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"testing"
)

func publicationRevision(n int64) *int64 { return &n }
func newPublicationTestStore(t *testing.T, pool *pgxpool.Pool) MerchantSecretStore {
	t.Helper()
	store, err := NewDBSecretStore(db.WrapPool(pool, ""))
	require.NoError(t, err)
	return store
}
func currentPublicationRevision(t *testing.T, s *Service, id merchant.ID, rail string) *int64 {
	t.Helper()
	cfg, err := s.GetPaymentProviderConfig(context.Background(), id, rail, "live")
	if errors.Is(err, ErrPaymentProviderNotFound) {
		return publicationRevision(0)
	}
	require.NoError(t, err)
	return publicationRevision(cfg.Revision)
}
