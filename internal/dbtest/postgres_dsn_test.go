//go:build integration

package dbtest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestMerchantPinnedDSNEncodesStartupOptionSpace(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := MerchantPinnedDSN(t, id)
	cfg, err := pgconn.ParseConfig(got)
	require.NoError(t, err)
	require.Equal(t, "-c app.merchant_id="+id.String(), cfg.RuntimeParams["options"])
}
