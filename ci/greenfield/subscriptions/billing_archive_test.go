//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// The portable billing archive's schema check classifies every subscription
// column the lifecycle added: an export is never refused for the schema
// itself (count 0), only for live rows it cannot move yet.
func TestBillingArchiveClassifiesLifecycleColumns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.toPeriodEnd()
	w.runRenewals()
	status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
	if status == http.StatusOK {
		return
	}
	var refusal struct {
		Error struct {
			Code     string `json:"code"`
			Metadata struct {
				Table string `json:"table"`
				Count int    `json:"count"`
			} `json:"metadata"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	require.Positive(t, refusal.Error.Metadata.Count, "a refusal names live rows, never an unclassified schema: %s", body)
}

// #1099: idempotency claims are not moved by the archive, but a request still
// running under a live claim holds the export back; settled and lapsed claims
// never do.
func TestBillingArchiveWaitsForALiveClaim(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	archiveRefusal := func() (string, int) {
		status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
		if status == http.StatusOK {
			return "", 0
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
					Count int    `json:"count"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table, refusal.Error.Metadata.Count
	}
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO openrails.idempotency_keys (merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
		SELECT id, 'checkout_session_create', k, s, gen_random_uuid(), CASE WHEN s = 'succeeded' THEN '{}'::jsonb END, now() + interval '1 hour', now() + interval '1 day'
		FROM openrails.merchants, (VALUES ('running', 'processing'), ('done', 'succeeded')) AS v(k, s) WHERE slug = $1`), w.slug)
	require.NoError(t, err)
	table, count := archiveRefusal()
	require.Equal(t, "idempotency_keys", table)
	require.Equal(t, 1, count, "only the live claim holds the export back")

	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE openrails.idempotency_keys SET lease_expires_at = now() - interval '1 second' WHERE idempotency_key = 'running'`))
	require.NoError(t, err)
	table, _ = archiveRefusal()
	require.NotEqual(t, "idempotency_keys", table, "a lapsed claim does not block the export")
}
