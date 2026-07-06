//go:build integration

package integrationharness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// #579 end-to-end replay of the client-facing Idempotency-Key header over the
// REAL Redis-backed Runtime.HTTPIdempotency store (not the in-memory fallback
// the internal/http/middleware unit tests exercise): two requests hitting the
// SAME standalone server must actually share the Redis-backed cache. Uses the
// merchant team-invite route (POST /v1/merchant/team/invites) — the simplest
// mutating route to authenticate against with this harness (owner bootstrap
// token); the middleware itself is entirely route-agnostic.
func TestIdempotencyKeyHTTPReplay(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	owner := surface.Token
	base := surface.BaseURL

	cp := embcp.Get(surface.App())
	require.NotNil(t, cp)
	core := cp.Core()
	require.NotNil(t, core)

	require.NotNil(t, surface.App().Runtime.RedisClient,
		"this replay test requires a Redis-backed Runtime.HTTPIdempotency store — "+
			"without Redis the middleware would silently exercise only the in-memory "+
			"fallback and the test would prove nothing about the shared store")

	_, email := makeUser(t, core, "idemphttp"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8])

	doPost := func(idemKey, bodyJSON string) (int, []byte, http.Header) {
		req, err := http.NewRequest(http.MethodPost, base+"/v1/merchant/team/invites", strings.NewReader(bodyJSON))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+owner)
		req.Header.Set("Idempotency-Key", idemKey)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, raw, resp.Header
	}

	key := uuid.NewString() // unique per run: sibling tests share the package Redis.
	bodyJSON := `{"email":"` + email + `","role":"viewer"}`

	status1, body1, headers1 := doPost(key, bodyJSON)
	require.Equalf(t, http.StatusCreated, status1, "first invite: %s", string(body1))
	require.Empty(t, headers1.Get("Idempotent-Replayed"), "the first (originating) call must not be marked replayed")

	status2, body2, headers2 := doPost(key, bodyJSON)
	require.Equal(t, status1, status2, "replay: %s", string(body2))
	require.Equal(t, string(body1), string(body2), "replay must return the byte-identical original response")
	require.Equal(t, "true", headers2.Get("Idempotent-Replayed"))

	// Same key, a DIFFERENT body -> the middleware's own 409, no second
	// business-level call (which would otherwise attempt to re-invite the same
	// email under a different role).
	differentBody := `{"email":"` + email + `","role":"owner"}`
	status3, body3, _ := doPost(key, differentBody)
	require.Equal(t, http.StatusConflict, status3, "reuse with different body: %s", string(body3))
	require.Contains(t, string(body3), `"idempotency_key_reuse"`)
}
