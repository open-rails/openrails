//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// An Idempotency-Key longer than the tables hold is invalid input, refused
// before any route runs.
func TestOversizedIdempotencyKeyIsInvalid(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	status, out := c.call(http.MethodPost, "/payment-methods", strings.Repeat("k", 256), map[string]any{})
	require.Equal(t, http.StatusBadRequest, status, "%v", out)
	require.Equal(t, "invalid_param", errorCode(out))
}
