//go:build integration

package critical_test

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/logx"
)

// records is every JSON record logs holds.
func (b *capturedLogs) records(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	lines := bufio.NewScanner(strings.NewReader(b.text()))
	lines.Buffer(nil, 1<<24)
	for lines.Scan() {
		var record map[string]any
		require.NoError(t, json.Unmarshal(lines.Bytes(), &record), lines.Text())
		out = append(out, record)
	}
	require.NoError(t, lines.Err())
	return out
}

func TestLogging(t *testing.T) {
	t.Run("OR-J04-S76/every-logger-the-engine-reaches-is-the-hosts", func(t *testing.T) {
		before := slog.Default()
		logs := &capturedLogs{}
		a := newAuthorityWorld(t, func(w *world) {
			w.deps.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		})
		// Composing the fleet registers every worker through addWorker,
		// which refuses one that reaches a nil logger.
		hostFleet(t, a.world)
		require.Empty(t, logx.Unset(a.engine))
		require.Same(t, before, slog.Default(), "the engine never changes the host's default logger")

		status, raw := a.request(t, http.MethodGet, "/v1/admin/customers/"+uuid.NewString(), a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		var refusal map[string]any
		for _, record := range logs.records(t) {
			if record["code"] == "customer_not_found" {
				refusal = record
			}
		}
		require.NotNil(t, refusal, "the host's logger received the refusal: %s", logs.text())
		require.Equal(t, "/v1/admin/customers/{customer_id}", refusal["route"])
		require.Equal(t, a.alpha.MerchantID().String(), refusal["merchant_id"])
		require.NotEmpty(t, refusal["request_id"])
	})
	t.Run("OR-J04-S76/the-standalone-server-boots-with-every-logger-set", func(t *testing.T) {
		// server.New, its meta routes included, with the real wiring.
		w := newStandaloneWorld(t)
		require.Empty(t, w.UnsetLoggers)
		for _, path := range []string{"/health/live", "/v1/config"} {
			status, raw := w.request(t, http.MethodGet, path, "", nil)
			require.Equal(t, http.StatusOK, status, "%s: %s", path, raw)
		}
	})
}
