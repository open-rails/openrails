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
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/stretchr/testify/require"
)

// reservationWorld is an actor allowed two destructive actions an hour, four
// credit grants it may revoke, and a fixture that stages the reservation a
// crash between admission and start leaves behind.
type reservationWorld struct {
	*authorityWorld
	grants             []billing.CreditGrant
	key                string
	actorIssuer, actor string
	actorKey           string
}

const reservationReason = "operator correction"

func newReservationWorld(t *testing.T) *reservationWorld {
	t.Helper()
	a := newAuthorityWorld(t, func(w *world) {
		w.limits = func(l *destructive.Limits) { l.ActorPerHour = 2 }
	})
	var in []billing.CreateCreditGrantParams
	for i := range 4 {
		in = append(in, billing.CreateCreditGrantParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Currency: "USD", Amount: 100, Source: "critical", SourceID: "reservation-" + strconv.Itoa(i)})
	}
	grants, err := a.alpha.CreateCreditGrants(t.Context(), in)
	require.NoError(t, err)
	return &reservationWorld{authorityWorld: a, grants: grants, key: roleKey(t, a, "support", billing.MerchantCustomerManage)}
}

// revoke revokes grant over HTTP, answering the status, Retry-After and body.
func (w *reservationWorld) revoke(t *testing.T, grant billing.CreditGrant) (int, string, []byte) {
	t.Helper()
	body, err := json.Marshal(billing.RevokeCreditGrantParams{Reason: reservationReason})
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+"/v1/admin/credit-grants/"+grant.ID.String()+"/revoke", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+w.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := w.server.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	var raw bytes.Buffer
	_, err = raw.ReadFrom(response.Body)
	require.NoError(t, err)
	return response.StatusCode, response.Header.Get("Retry-After"), raw.Bytes()
}

func (w *reservationWorld) actions() string {
	return pgx.Identifier{w.schema, "destructive_actions"}.Sanitize()
}

// learnActor reads the actor of the revocation of grant.
func (w *reservationWorld) learnActor(t *testing.T, grant billing.CreditGrant) {
	t.Helper()
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT actor_issuer, actor_id, actor_key FROM "+w.actions()+" WHERE merchant_id = $1 AND target = $2",
		w.alpha.MerchantID().UUID(), grant.ID.UUID().String()).Scan(&w.actorIssuer, &w.actor, &w.actorKey))
}

// reserve stages the reservation a crash leaves, admitted now; for a grant,
// under the key its revocation by the world's actor uses.
func (w *reservationWorld) reserve(t *testing.T, grant *billing.CreditGrant) uuid.UUID {
	t.Helper()
	target, actionKey := uuid.NewString(), "crashed-"+uuid.NewString()
	if grant != nil {
		target = grant.ID.UUID().String()
		actionKey = "credit-revoke:" + target + ":actor:" + w.actorKey + ":terms:" + destructive.Digest(struct{ Reason string }{reservationReason})
	}
	id := uuid.New()
	_, err := w.pool.Exec(t.Context(), "INSERT INTO "+w.actions()+" (merchant_id, id, kind, target, action_key, request_digest, origin, actor_issuer, actor_id, actor_key, units, admitted_at, state) VALUES ($1, $2, 'revoke_credit', $3, $4, $5, 'staff', $6, $7, $8, 1, $9, 'reserved')",
		w.alpha.MerchantID().UUID(), id, target, actionKey, destructive.Digest(struct{ Reason string }{reservationReason}), w.actorIssuer, w.actor, w.actorKey, w.clock.Now().UTC())
	require.NoError(t, err)
	return id
}

func (w *reservationWorld) state(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var state string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT state FROM "+w.actions()+" WHERE merchant_id = $1 AND id = $2", w.alpha.MerchantID().UUID(), id).Scan(&state))
	return state
}

func TestDestructiveReservations(t *testing.T) {
	t.Run("OR-J34-S32/a-reservation-that-never-starts-stops-holding-budget", func(t *testing.T) {
		w := newReservationWorld(t)
		status, _, raw := w.revoke(t, w.grants[0])
		require.Equal(t, http.StatusOK, status, string(raw))
		w.learnActor(t, w.grants[0])

		// Past ReservationLife a reservation that never started holds no
		// budget: the actor's second revocation of the hour still fits.
		w.reserve(t, nil)
		w.clock.Advance(destructive.ReservationLife + time.Minute)
		status, _, raw = w.revoke(t, w.grants[1])
		require.Equal(t, http.StatusOK, status, string(raw))

		// A reservation refused at its start by a hold ends without effect:
		// the third revocation opens the actor's hold, and the retry of the
		// reserved one is refused at its start, with when to look again.
		held := w.reserve(t, &w.grants[2])
		status, retry, raw := w.revoke(t, w.grants[3])
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Equal(t, strconv.Itoa(int(destructive.HeldRetryAfter/time.Second)), retry)
		status, retry, raw = w.revoke(t, w.grants[2])
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.NotEmpty(t, retry)
		require.Equal(t, "no_effect", w.state(t, held))
		grant, err := w.alpha.GetCreditGrant(t.Context(), w.grants[2].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, grant.State)
	})
	t.Run("OR-J34-S33/a-reservation-started-past-its-life-is-admitted-again", func(t *testing.T) {
		w := newReservationWorld(t)
		status, _, raw := w.revoke(t, w.grants[0])
		require.Equal(t, http.StatusOK, status, string(raw))
		w.learnActor(t, w.grants[0])
		// The revocation of grant 1 was admitted, then never started.
		stale := w.reserve(t, &w.grants[1])
		w.clock.Advance(destructive.ReservationLife + time.Minute)
		status, _, raw = w.revoke(t, w.grants[2])
		require.Equal(t, http.StatusOK, status, string(raw))
		// Its retry finds the reservation, which no longer holds budget, so
		// it is admitted again: the actor's two of the hour are spent, and it
		// is refused rather than overspending.
		status, _, raw = w.revoke(t, w.grants[1])
		require.Equal(t, http.StatusTooManyRequests, status, string(raw))
		require.Equal(t, "no_effect", w.state(t, stale))
		grant, err := w.alpha.GetCreditGrant(t.Context(), w.grants[1].ID)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, grant.State)
	})
}
