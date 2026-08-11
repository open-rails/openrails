//go:build integration

// or#900 items 2 and 3: two answers the engine already gives that the SDK
// dropped on the floor.
//
//	item 2 — CreditTransaction.Replayed. or#892 made every money write report
//	         applied-vs-replayed and the handlers serialize it, but the SDK type
//	         had no field, so a consumer could not read it and kept its own
//	         claim table to re-derive it.
//	item 3 — ErrIdempotencyKeyReused. The refusal ("this key already committed a
//	         DIFFERENT amount") lived in internal/ with no exported name and no
//	         wire mapping, so a host saw an opaque 4xx/5xx and could not tell its
//	         own bug from an engine fault.
//
// Both are asserted on BOTH transports — the REMOTE client against the real
// standalone server, and the in-process transport of the embedded host — because
// a fix on one transport and not the other is the drift the unified SDK exists
// to prevent.
package embed_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestOr900_ReplayedAndIdempotencyConflictCrossBothTransports(t *testing.T) {
	ctx := context.Background()
	currency := money.DefaultCurrency

	h := integrationharness.New(t, ctx)
	embedded := h.StartEmbeddedHost(currency)
	standalone := h.StartStandalone(currency)
	pool := h.Pool()

	newPayer := func() uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.customers (id, merchant_id, issuer, subject, created_at, last_seen_at)
			VALUES ($1, $2, 'or900', $3, now(), now())`,
			id, dbtest.TestMerchantID.UUID(), id.String())
		require.NoError(t, err)
		return id
	}

	transports := []struct {
		name   string
		client openrails.Client
	}{
		{"in-process", embedded.Runtime().Client(embed.WithCurrency(currency))},
		{"remote", standalone.Client()},
	}

	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			payer := newPayer()
			pid := openrails.CustomerID(payer)
			invoker := "user:or900-" + uuid.NewString()

			// --- item 2: applied vs replayed on the SDK type ------------------
			req := openrails.DepositCreditsRequest{
				CustomerID: &pid,
				Invoker:    invoker,
				Currency:   currency,
				Amount:     1_000_000,
				Source:     "or900",
				SourceID:   uuid.NewString(),
			}
			first, err := tr.client.DepositCredits(ctx, req)
			require.NoError(t, err)
			require.False(t, first.Replayed, "a first deposit APPLIES — money moved in this call")

			again, err := tr.client.DepositCredits(ctx, req)
			require.NoError(t, err, "a replayed deposit is answered, not rejected")
			require.True(t, again.Replayed,
				"or#900: the engine reports the replay and the SDK must surface it, or a consumer double-counts")
			require.Equal(t, first.Amount, again.Amount, "the replay describes the movement that already landed")

			// --- item 3: the reused-key refusal reaches the host --------------
			usageSourceID := uuid.NewString()
			usage := openrails.UsageReport{
				CustomerID: payer.String(),
				Invoker:    invoker,
				Currency:   currency,
				EventType:  "or900_event",
				Amount:     50_000,
				Source:     "or900",
				SourceID:   usageSourceID,
			}
			require.NoError(t, tr.client.RecordUsage(ctx, usage), "first usage charge applies")

			// Same key, DIFFERENT charging terms: a caller bug, and it must be
			// named as one rather than answered with the original amount.
			conflicting := usage
			conflicting.Amount = 90_000
			err = tr.client.RecordUsage(ctx, conflicting)
			require.Error(t, err, "replaying a key with a changed amount must refuse")
			require.ErrorIs(t, err, openrails.ErrIdempotencyKeyReused,
				"or#900: the wire error must map back to the exported sentinel on this transport")

			var se *openrails.StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, http.StatusConflict, se.Status, "a reused key is a conflict, not a 500")
			require.Contains(t, se.Message, "idempotency_key_reused")
			require.Contains(t, se.Message, "90000", "the detail names what the retry asked for")
			require.Contains(t, se.Message, "50000", "and what the key already committed")

			// Replaying it UNCHANGED is still fine — the refusal is about the
			// changed terms, not about retrying.
			require.NoError(t, tr.client.RecordUsage(ctx, usage), "an unchanged retry is still idempotent")
		})
	}
}

// TestOr900_ServiceFacadeExportsTheIdempotencySentinel is the in-process half of
// item 3: a host that holds *service.Service (not the SDK client) must be able
// to name the same refusal without importing internal/.
func TestOr900_ServiceFacadeExportsTheIdempotencySentinel(t *testing.T) {
	require.True(t, errors.Is(billingservice.ErrIdempotencyKeyReused, money.ErrIdempotencyKeyReused),
		"pkg/service must re-export the money sentinel itself, not a look-alike")
}
