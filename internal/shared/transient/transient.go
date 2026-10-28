// Package transient tells failures a later attempt can outlive (a peer that
// did not answer, answered busy or failed, a lost database connection) from
// those it cannot.
package transient

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrUnavailable marks a peer that did not answer, or answered that it is
// busy or failing (429, 5xx).
var ErrUnavailable = errors.New("the service is unavailable")

// Status reports whether an HTTP status is the peer busy or failing.
func Status(code int) bool { return code == 429 || code >= 500 }

// Is reports whether a later attempt may succeed where err failed.
func Is(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	var connect *pgconn.ConnectError
	if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) || errors.As(err, &connect) || pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return true
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		// Connection, transaction rollback, resources, operator intervention,
		// system errors, and a lock not available now.
		for _, class := range []string{"08", "40", "53", "57", "58"} {
			if strings.HasPrefix(pg.Code, class) {
				return true
			}
		}
		return pg.Code == "55P03"
	}
	return false
}
