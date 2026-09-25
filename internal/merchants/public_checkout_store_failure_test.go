package merchants

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestStoreFailure(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"database error", fmt.Errorf("resolve: %w", &pgconn.PgError{Code: "57014"}), true},
		{"request cancelled", fmt.Errorf("resolve: %w", context.Canceled), true},
		{"request deadline", context.DeadlineExceeded, true},
		{"credential backend", errors.New("secret backend unavailable"), false},
		{"settings", errors.New("NMI endpoint_deployment must be gateway or sandbox"), false},
	} {
		if got := storeFailure(c.err); got != c.want {
			t.Errorf("%s: storeFailure = %v, want %v", c.name, got, c.want)
		}
	}
}
