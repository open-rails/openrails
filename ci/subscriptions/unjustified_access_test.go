//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Live access whose subscription does not exist is a freeloader: the sweep
// raises derive.access.unjustified, the freeloaders gauge counts it, and the
// finding clears once the window is revoked.
func TestUnjustifiedAccessFindingCountsAndClears(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	member := w.membership("freeload:access", 9_990_000)
	now := w.clock.Now()
	grant, access := uuid.New(), uuid.New()
	missing := uuid.NewString()
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.grants (merchant_id, id, customer_id, product_id, kind, source_type, source_id, event, spec_snapshot, starts_at, ends_at)
		SELECT p.merchant_id, $2, $3, p.id, 'access', 'subscription', $4, 'grant', NULL, $5, $6 FROM billing.products p WHERE p.id = $1`),
		member.ProductID.UUID(), grant, uuid.MustParse(c.id), missing, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.product_access (merchant_id, id, customer_id, product_id, grant_id, source_type, source_id, starts_at)
		SELECT p.merchant_id, $2, $3, p.id, $4, 'subscription', $5, $6 FROM billing.products p WHERE p.id = $1`),
		member.ProductID.UUID(), access, uuid.MustParse(c.id), grant, missing, now.Add(-48*time.Hour))
	require.NoError(t, err)

	freeloaders := func() int64 {
		t.Helper()
		status, body := w.staffJSON(http.MethodPost, "/v1/admin/metrics/query", map[string]any{"measures": []string{"freeloaders"}, "range": nowRange()})
		require.Equal(t, http.StatusOK, status, "%v", body)
		row := body["rows"].([]any)[0].([]any)
		return number(t, row[len(row)-1])
	}
	w.converge()
	require.Equal(t, []string{"product_access:" + access.String()}, w.openFindings("derive.access.unjustified"))
	require.EqualValues(t, 1, freeloaders(), "the gauge counts the freeloader")

	require.NoError(t, w.client[embedded].DeleteProductAccess(t.Context(), c.cid(), billing.ProductAccessID(access)))
	w.converge()
	require.Empty(t, w.openFindings("derive.access.unjustified"), "the finding clears with the window")
	require.Zero(t, freeloaders())
}
