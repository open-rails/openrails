//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// listAll pages the admin subscription list at query, size rows a page, and
// answers the ids in order and the cursors it followed.
func (w *world) listAll(query url.Values, size int) ([]string, []string) {
	w.t.Helper()
	var ids, cursors []string
	cursor := ""
	for {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("limit", strconv.Itoa(size))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		status, page := w.staffJSON(http.MethodGet, "/v1/admin/subscriptions?"+q.Encode(), nil)
		require.Equal(w.t, http.StatusOK, status, "%v", page)
		ids = append(ids, idsOf(w.t, page)...)
		next, _ := page["next_cursor"].(string)
		if next == "" {
			return ids, cursors
		}
		cursors = append(cursors, next)
		cursor = next
	}
}

// The admin subscription list orders by creation (newest first), by period
// end (soonest first) and by cancellation (latest first). Paging keeps every
// row exactly once; a cursor names its order; an unknown order is refused.
func TestAdminSubscriptionsSort(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	type member struct {
		id    billing.SubscriptionID
		hours int
	}
	var members []member
	for i, hours := range []int{24 * 10, 24 * 3, 24 * 30, 24 * 7} {
		price := w.membershipEvery("content:sort-"+string(rune('a'+i)), 1_000_000, hours)
		c := w.newCustomer()
		members = append(members, member{c.subscribe(embedded, "stripe", price.ID.String(), "content:sort-"+string(rune('a'+i)), c.saveCard("stripe", visa)), hours})
		w.advance(time.Minute)
	}
	for _, m := range []member{members[2], members[0]} {
		_, err := w.client[embedded].CancelSubscription(t.Context(), m.id, billing.CancelSubscriptionParams{Reason: "sort"})
		require.NoError(t, err)
		w.advance(time.Minute)
	}
	all := make([]string, len(members))
	for i, m := range members {
		all[i] = m.id.String()
	}

	newest, cursors := w.listAll(url.Values{}, 3)
	require.Equal(t, []string{all[3], all[2], all[1], all[0]}, newest, "newest first by default")
	require.Len(t, cursors, 1)
	byCreation, _ := w.listAll(url.Values{"sort": {"created_at"}}, 3)
	require.Equal(t, newest, byCreation)

	byEnd, endCursors := w.listAll(url.Values{"sort": {"current_period_ends_at"}}, 3)
	require.Equal(t, []string{all[1], all[3], all[0], all[2]}, byEnd, "soonest period end first: 3, 7, 10 and 30 days")
	require.Len(t, endCursors, 1)

	byCancel, cancelCursors := w.listAll(url.Values{"sort": {"canceled_at"}}, 1)
	require.Equal(t, []string{all[0], all[2]}, byCancel, "only the canceled, the latest first")
	require.Len(t, cancelCursors, 1)

	renewingSoon, _ := w.listAll(url.Values{"sort": {"current_period_ends_at"}, "status": {"active"},
		"expires_before": {w.clock.Now().UTC().AddDate(0, 0, 8).Format(time.DateOnly)}}, 2)
	for _, id := range renewingSoon {
		require.Contains(t, []string{all[1], all[3]}, id, "active and ending within the week")
	}
	require.True(t, slices.Contains(renewingSoon, all[1]) && slices.Contains(renewingSoon, all[3]))

	status, body := w.staffJSON(http.MethodGet, "/v1/admin/subscriptions?sort=current_period_ends_at&cursor="+url.QueryEscape(cancelCursors[0]), nil)
	require.Equal(t, http.StatusBadRequest, status, "a cursor of another order: %v", body)
	require.Equal(t, "invalid_cursor", body["error"].(map[string]any)["code"])
	status, body = w.staffJSON(http.MethodGet, "/v1/admin/subscriptions?sort=created_at&cursor="+url.QueryEscape(endCursors[0]), nil)
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	require.Equal(t, "invalid_cursor", body["error"].(map[string]any)["code"])
	status, body = w.staffJSON(http.MethodGet, "/v1/admin/subscriptions?sort=expires_at", nil)
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	refusal := body["error"].(map[string]any)
	require.Equal(t, "invalid_query", refusal["code"])
	require.Equal(t, "sort", refusal["param"])

	for _, tp := range []topology{embedded, remote} {
		page, err := w.client[tp].ListSubscriptions(t.Context(), billing.SubscriptionListParams{Sort: billing.SubscriptionSortCanceledAt})
		require.NoError(t, err)
		var got []string
		for _, s := range page.Items {
			got = append(got, s.ID.String())
		}
		require.Equal(t, []string{all[0], all[2]}, got, tp)
	}
}
