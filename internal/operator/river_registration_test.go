package operator

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
)

func TestAttachRefusesAlreadyConstructedRiver(t *testing.T) {
	application := &app.App{Runtime: &app.Runtime{RiverClient: &river.Client[pgx.Tx]{}}}
	err := Attach(context.Background(), application, &config.Config{}, nil, nil)
	require.ErrorContains(t, err, "attach before River initialization")
	require.Nil(t, application.ControlPlane, "late attach must fail before building identity resources")
}
