//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/stretchr/testify/require"
)

// The canonical idempotency key is a UUID, and #491/#495/#494 require a
// client-supplied key on money mutations. Before the card detector learned card
// grouping, a bare uuid.NewString() was read as a card number and refused with
// "idempotency key contains invalid card input" about 1 request in 480
// (417/200000 measured at 2a28bcab1) — the dashes were treated as PAN
// formatting, so digits ran across the UUID's groups.
//
// This is the real surface: real HTTP against the standalone server, real
// Postgres behind it. A key that used to be refused must now be answered
// exactly like an ordinary key, and a real card number pasted anywhere in the
// request — the header included — must still be refused.
func TestCheckoutIdempotencyKeyIsNotCardInputHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	caller := surface.RegisterDelegatedCaller(
		"pankey-"+strings.ReplaceAll(uuid.NewString(), "-", ""),
		dbtest.TestMerchantSlug,
		uuid.NewString(),
		nil,
	)
	token := caller.Token

	type refusal struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	post := func(t *testing.T, key string, body map[string]any) (int, refusal, string) {
		t.Helper()
		var buf bytes.Buffer
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/me/checkout", &buf)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		require.NoError(t, testauth.Authorize(req, token))
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var out refusal
		_ = json.Unmarshal(raw, &out)
		require.NotEqual(t, http.StatusTooManyRequests, resp.StatusCode, "rate limited; this proof needs a served response")
		return resp.StatusCode, out, string(raw)
	}
	// One body, reused for every key: the answer is the route's own, and what
	// must not vary is the key. No rail is named, so the request goes through
	// CreateSession's own gates (the card scan among them) rather than stopping
	// at the handler's named-PSP pre-gate.
	body := func() map[string]any {
		return map[string]any{
			"price_id": "price_" + uuid.NewString(),
			"payment":  map[string]any{"payment_token": "tok_" + uuid.NewString()},
		}
	}

	// The control: a key nothing has ever objected to.
	wantStatus, wantErr, wantRaw := post(t, uuid.NewString(), body())
	require.NotContains(t, wantRaw, "card input", "control key must not be read as card input")

	// Keys that WERE refused: pinned UUIDs whose digit groups form a Luhn-valid
	// run, and the typed ids built from one.
	for _, key := range []string{
		"a544fda7-1958-4199-9417-3263a6c4b369",
		"72967ae8-6314-4231-9276-8176f5be4867",
		"805b6084-0619-4770-9a20-90104f447f64",
		"abcdefab-cdef-4abc-8111-111111111112",
		"sub_a4111111-1111-4111-8119-abcdefabcdef",
		"1758153600123456786", // an epoch-nanosecond key, also Luhn-valid
	} {
		status, gotErr, raw := post(t, key, body())
		require.NotContainsf(t, raw, "card input", "key %q was refused as card input", key)
		require.Equalf(t, wantStatus, status, "key %q answered differently from the control: %s", key, raw)
		require.Equalf(t, wantErr.Error.Code, gotErr.Error.Code, "key %q answered with a different code: %s", key, raw)
		require.Equalf(t, wantErr.Error.Message, gotErr.Error.Message, "key %q answered with a different message: %s", key, raw)
	}

	// The key really is claimed in the store, not merely waved through: the
	// same key twice answers the way a replayed key does, not the way a fresh
	// one does. Proven against the control so the expectation is the route's.
	replayKey := "463d4942-14ef-4eec-9436-151088257692"
	controlKey := uuid.NewString()
	firstControlStatus, firstControlErr, _ := post(t, controlKey, body())
	secondControlStatus, secondControlErr, controlRaw := post(t, controlKey, body())
	require.NotEqualf(t, firstControlErr.Error.Code, secondControlErr.Error.Code,
		"the second call must be answered by the idempotency store, or this proves nothing: %s", controlRaw)
	firstReplayStatus, firstReplayErr, firstReplayRaw := post(t, replayKey, body())
	secondReplayStatus, secondReplayErr, replayRaw := post(t, replayKey, body())
	require.Equalf(t, firstControlStatus, firstReplayStatus, "first call: %s", firstReplayRaw)
	require.Equalf(t, firstControlErr.Error.Code, firstReplayErr.Error.Code, "first call: %s", firstReplayRaw)
	require.Equalf(t, secondControlStatus, secondReplayStatus, "replayed tripping key: %s", replayRaw)
	require.Equalf(t, secondControlErr.Error.Code, secondReplayErr.Error.Code, "replayed tripping key: %s", replayRaw)

	// A real card number is still refused — in the header, and in a nested
	// body field, and in a metadata key.
	for _, card := range []string{"4111111111111111", "5555 5555 5555 4444", "3782-822463-10005"} {
		status, _, raw := post(t, card, body())
		require.Equalf(t, http.StatusBadRequest, status, "card %q as an idempotency key: %s", card, raw)
		require.Containsf(t, raw, "card input", "card %q as an idempotency key: %s", card, raw)

		nested := body()
		nested["payment"].(map[string]any)["name_on_card"] = "Cardholder " + card
		status, _, raw = post(t, uuid.NewString(), nested)
		require.Equalf(t, http.StatusBadRequest, status, "card %q in payment.name_on_card: %s", card, raw)
		require.Containsf(t, raw, "card-number-shaped", "card %q in payment.name_on_card: %s", card, raw)

		tagged := body()
		tagged["metadata"] = map[string]any{"note": card}
		status, _, raw = post(t, uuid.NewString(), tagged)
		require.Equalf(t, http.StatusBadRequest, status, "card %q in metadata: %s", card, raw)
		require.Containsf(t, raw, "card-number-shaped", "card %q in metadata: %s", card, raw)

		keyed := body()
		keyed["metadata"] = map[string]any{card: "note"}
		status, _, raw = post(t, uuid.NewString(), keyed)
		require.Equalf(t, http.StatusBadRequest, status, "card %q as a metadata key: %s", card, raw)
		require.Containsf(t, raw, "card-number-shaped", "card %q as a metadata key: %s", card, raw)
	}
}
