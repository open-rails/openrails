//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Mixed source pages preserve tied timestamps/IDs and distinguish answers
// from operation progress. Fixtures never contact a real payment provider.
func TestSubscriptionHistoryFactsAndPaging(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	w.settle()
	id := uuid.New()
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	run := func(sql string, args ...any) {
		t.Helper()
		_, err := w.pool.Exec(t.Context(), w.q(sql), args...)
		require.NoError(t, err)
	}
	run(`INSERT INTO billing.subscription_status_transitions
 (merchant_id,id,subscription_id,from_status,to_status,occurred_at,decision)
 SELECT merchant_id,$2,id,'active','past_due',$3,'renewal_declined'
 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), id, at)
	run(`INSERT INTO billing.payment_attempts
 (merchant_id,id,customer_id,psp_id,rail,kind,owner,card_entry,source,observed_via,
 category,reason,action,amount,currency,attempted_at,created_at,subscription_id,step,response_text)
 SELECT merchant_id,$2,customer_id,psp_id,rail,'initial','engine','saved','external','pull',
 'issuer_soft','insufficient_funds','retry',1000000,'USD',$3,$4,id,'history-fixture','PRIVATE PROVIDER MESSAGE'
 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), id, at, at.Add(time.Minute))
	operation := uuid.New()
	run(`INSERT INTO billing.provider_intents
 (merchant_id,id,rail,psp_id,intent_type,subscription_id,idempotency_key,status,next_attempt_at,origin)
 SELECT merchant_id,$2,rail,psp_id,'manual_rebill',id,($2::uuid)::text,'failed_terminal',$3,'system'
 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), operation, at)
	run(`INSERT INTO billing.provider_mutation_logs
 (merchant_id,id,rail,psp_id,provider_intent_id,intent_type,attempt,phase,reason,evidence,created_at)
 SELECT merchant_id,$2,rail,psp_id,$3,'manual_rebill',1,'unknown',
 'PRIVATE OPERATION MESSAGE','{"token":"PRIVATE TOKEN"}',$4
 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), id, operation, at)
	run(`INSERT INTO billing.provider_mutation_logs
 (merchant_id,rail,psp_id,provider_intent_id,intent_type,attempt,phase,created_at)
 SELECT merchant_id,rail,psp_id,$2,'manual_rebill',1,'parked',$3
 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), operation, at.Add(time.Second))

	run(`INSERT INTO billing.subscription_billing_changes
 (merchant_id,id,subscription_id,occurred_at,kind,from_retry_attempts,to_retry_attempts)
 SELECT merchant_id,$2,id,$3,'retry_schedule',0,1 FROM billing.subscriptions WHERE id=$1`, subUUID(e.sub), id, at)
	before := len(w.railLedger("stripe"))
	full, err := w.client[embedded].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 500})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(full.Events.Items), 5)
	require.Empty(t, full.Events.Next)
	require.Equal(t, "parked", full.Events.Items[0].Operation.Phase)
	require.Equal(t, "retry_schedule", full.Events.Items[1].Change.Kind)
	require.Equal(t, "unknown", full.Events.Items[2].Operation.Phase)
	require.Equal(t, operation.String(), full.Events.Items[2].Operation.ID)
	require.Nil(t, full.Events.Items[2].Attempt, "uncertainty is not a provider decline")
	require.Equal(t, "insufficient_funds", full.Events.Items[3].Attempt.Reason)
	require.True(t, at.Equal(full.Events.Items[3].At))
	require.True(t, at.Add(time.Minute).Equal(full.Events.Items[3].RecordedAt))
	require.Equal(t, "renewal_declined", full.Events.Items[4].Transition.Decision)
	require.Contains(t, full.Coverage.Gaps, "unavailable_attribution")

	var paged []billing.SubscriptionHistoryEntry
	cursor := ""
	for n := 0; n < 100; n++ {
		page, err := w.client[remote].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 1, Cursor: cursor})
		require.NoError(t, err)
		paged = append(paged, page.Events.Items...)
		cursor = page.Events.Next
		if cursor == "" {
			break
		}
	}
	require.Empty(t, cursor, "pagination must terminate")
	require.Equal(t, full.Events.Items, paged)
	raw, err := json.Marshal(full)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "PRIVATE")
	require.NotContains(t, string(raw), "next_retry")
	require.Equal(t, before, len(w.railLedger("stripe")), "reading never charges")
}

func TestSubscriptionHistoryAccessAndCursorScope(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	w.settle()
	_, err := w.client[embedded].CancelSubscription(t.Context(), e.sub, billing.CancelSubscriptionParams{Reason: "history test"})
	require.NoError(t, err)
	first, err := w.client[remote].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, first.Events.Next)
	path := "/v1/admin/subscriptions/" + e.sub.String() + "/history"
	status, body := w.merchantCall(w.auth.token(t, "reader"), http.MethodGet, path)
	require.Equal(t, http.StatusOK, status, body)
	status, body = w.merchantCall(w.auth.token(t, "nobody"), http.MethodGet, path)
	require.Equal(t, http.StatusForbidden, status, body)
	other := w.rival()
	status, answer := w.merchantJSONAt(other.server.URL, other.auth.token(t, "staff"), http.MethodGet, path, nil)
	require.Equal(t, http.StatusNotFound, status, "%v", answer)

	second := enroll(t, w, "nmi", embedded)
	for _, cursor := range []string{"not-a-cursor", first.Events.Next} {
		_, err = w.client[remote].GetSubscriptionHistory(t.Context(), second.sub, billing.PageRequest{Cursor: cursor})
		require.Error(t, err)
		var response *billing.StatusError
		require.ErrorAs(t, err, &response)
		require.Equal(t, "invalid_cursor", response.Code)
	}
	_, err = w.client[remote].GetSubscriptionHistory(t.Context(), e.sub, billing.PageRequest{Limit: 501})
	require.Error(t, err)
	var response *billing.StatusError
	require.ErrorAs(t, err, &response)
	require.Equal(t, "invalid_query", response.Code)
	_, err = w.client[embedded].GetSubscriptionHistory(t.Context(), billing.SubscriptionID(uuid.New()), billing.PageRequest{})
	require.Error(t, err)
	require.ErrorIs(t, err, billing.ErrNotFound)
}
