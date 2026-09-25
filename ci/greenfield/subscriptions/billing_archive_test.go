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
