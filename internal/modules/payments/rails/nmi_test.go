package rails

import (
	"testing"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestSameRail(t *testing.T) {
	t.Parallel()

	require.True(t, SameRail(models.Rail(" Nmi "), models.RailNMI))
	require.False(t, SameRail(models.RailNMI, models.Rail("other_nmi")))
	require.False(t, SameRail(models.Rail(""), models.RailNMI))
}
