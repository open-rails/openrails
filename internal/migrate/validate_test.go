package migrate

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

func TestValidationConnectivityIsNotPending(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("offline")}, &pgconn.PgError{Code: "57P01", Message: "restarting"}} {
		err := validationError(cause)
		require.ErrorIs(t, err, cause)
		require.NotErrorIs(t, err, ErrMigrationsRequired)
	}
	missing := &pgconn.PgError{Code: "42P01", Message: "missing relation"}
	err := validationError(missing)
	require.ErrorIs(t, err, ErrMigrationsRequired)
	require.ErrorIs(t, err, missing)
}
