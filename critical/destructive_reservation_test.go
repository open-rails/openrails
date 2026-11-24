//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/stretchr/testify/require"
)

func TestDestructiveReservations(t *testing.T) {
	t.Run("OR-J34-S32/a-reservation-that-never-starts-stops-holding-budget", func(t *testing.T) {
		a := newAuthorityWorld(t, func(w *world) {
			w.cfg.DestructiveActionLimits = openrails.DestructiveActionLimitsConfig{ActorPerHour: 2}
		})
		const reason = "operator correction"
		var in []billing.CreateCreditGrantParams
		for i := range 4 {
			in = append(in, billing.CreateCreditGrantParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Currency: "USD", Amount: 100, Source: "critical", SourceID: "reservation-" + strconv.Itoa(i)})
		}
		grants, err := a.alpha.CreateCreditGrants(t.Context(), in)
		require.NoError(t, err)
		key := roleKey(t, a, "support", billing.MerchantCustomerManage)
		revoke := func(grant billing.CreditGrant) (int, string, []byte) {
			t.Helper()
			body, err := json.Marshal(billing.RevokeCreditGrantParams{Reason: reason})
			require.NoError(t, err)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/admin/credit-grants/"+grant.ID.String()+"/revoke", bytes.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+key)
			request.Header.Set("Content-Type", "application/json")
			response, err := a.server.Client().Do(request)
			require.NoError(t, err)
			defer response.Body.Close()
			var raw bytes.Buffer
			_, err = raw.ReadFrom(response.Body)
			require.NoError(t, err)
			return response.StatusCode, response.Header.Get("Retry-After"), raw.Bytes()
		}
		actions := pgx.Identifier{a.schema, "destructive_actions"}.Sanitize()
		status, _, raw := revoke(grants[0])
		require.Equal(t, http.StatusOK, status, string(raw))
		var issuer, actorID, actorKey string
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT actor_issuer, actor_id, actor_key FROM "+actions+" WHERE merchant_id = $1 AND target = $2",
			a.alpha.MerchantID().UUID(), grants[0].ID.UUID().String()).Scan(&issuer, &actorID, &actorKey))
		// A crash between admission and start leaves a reservation behind.
		reserve := func(target, actionKey string) uuid.UUID {
			t.Helper()
			id := uuid.New()
			_, err := a.pool.Exec(t.Context(), "INSERT INTO "+actions+" (merchant_id, id, kind, target, action_key, request_digest, origin, actor_issuer, actor_id, actor_key, units, admitted_at, state) VALUES ($1, $2, 'revoke_credit', $3, $4, $5, 'staff', $6, $7, $8, 1, $9, 'reserved')",
				a.alpha.MerchantID().UUID(), id, target, actionKey, destructive.Digest(struct{ Reason string }{reason}), issuer, actorID, actorKey, a.clock.Now().UTC())
			require.NoError(t, err)
			return id
		}

		// Past ReservationLife a reservation that never started holds no
		// budget: the actor's second revocation of the hour still fits.
		reserve(uuid.NewString(), "crashed-"+uuid.NewString())
		a.clock.Advance(destructive.ReservationLife + time.Minute)
		status, _, raw = revoke(grants[1])
		require.Equal(t, http.StatusOK, status, string(raw))

		// A reservation refused at its start by a hold ends without effect:
		// the third revocation opens the actor's hold, and the retry of the
		// reserved one is refused at its start, with when to look again.
		target := grants[2].ID.UUID().String()
		held := reserve(target, "credit-revoke:"+target+":actor:"+actorKey+":terms:"+destructive.Digest(struct{ Reason string }{reason}))
		status, retry, raw := revoke(grants[3])
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Equal(t, strconv.Itoa(int(destructive.HeldRetryAfter/time.Second)), retry)
		status, retry, raw = revoke(grants[2])
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.NotEmpty(t, retry)
		var state string
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT state FROM "+actions+" WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), held).Scan(&state))
		require.Equal(t, "no_effect", state)
		grant, err := a.alpha.GetCreditGrant(t.Context(), grants[2].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, grant.State)
	})
}
