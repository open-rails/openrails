//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// The operator's merchant directory, worker health and lockouts are the
// server's Go API (the openrails CLI's merchants, workers and admin-lockouts
// commands) with no HTTP route. Soft delete leaves the directory and the
// merchant's credentials resolving nothing; restore brings both back.
func TestOperatorDirectoryAndWorkers(t *testing.T) {
	f := newFixture(t)
	cp := f.newVaultServer(t, nil)
	ctx := t.Context()
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	prefix := uniqueName("dir")
	first, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: prefix + "-a", DisplayName: "First"})
	require.NoError(t, err)
	second, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: prefix + "-b"})
	require.NoError(t, err)
	key := merchantKey(t, cp, first.MerchantID, "owner")
	findings := func() int { return call(t, handler, key.Secret, http.MethodGet, "/v1/admin/findings", "", nil).Code }
	require.Equal(t, http.StatusOK, findings())

	list := func(params billing.MerchantListParams) []billing.Merchant {
		t.Helper()
		params.Query = prefix
		page, err := cp.ListMerchants(ctx, params)
		require.NoError(t, err)
		return page.Items
	}
	ids := func(ms []billing.Merchant) []billing.MerchantID {
		out := []billing.MerchantID{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	require.Equal(t, []billing.MerchantID{second.MerchantID, first.MerchantID}, ids(list(billing.MerchantListParams{})), "newest first")
	page, err := cp.ListMerchants(ctx, billing.MerchantListParams{PageRequest: billing.PageRequest{Limit: 1}, Query: prefix})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.NotEmpty(t, page.Next)
	rest, err := cp.ListMerchants(ctx, billing.MerchantListParams{PageRequest: billing.PageRequest{Limit: 1, Cursor: page.Next}, Query: prefix})
	require.NoError(t, err)
	require.Equal(t, []billing.MerchantID{first.MerchantID}, ids(rest.Items))
	_, err = cp.ListMerchants(ctx, billing.MerchantListParams{PageRequest: billing.PageRequest{Cursor: "x"}})
	require.Error(t, err)
	_, err = cp.ListMerchants(ctx, billing.MerchantListParams{Statuses: []billing.MerchantStatus{"retired"}})
	require.ErrorIs(t, err, billing.ErrInvalid)

	deleted, err := cp.DeleteMerchant(ctx, first.MerchantID)
	require.NoError(t, err)
	require.Equal(t, billing.MerchantDeleted, deleted.Status)
	require.NotNil(t, deleted.DeletedAt)
	again, err := cp.DeleteMerchant(ctx, first.MerchantID)
	require.NoError(t, err)
	require.Equal(t, deleted.DeletedAt, again.DeletedAt, "deleting a deleted merchant changes nothing")
	require.Equal(t, []billing.MerchantID{second.MerchantID}, ids(list(billing.MerchantListParams{})))
	require.Equal(t, []billing.MerchantID{first.MerchantID}, ids(list(billing.MerchantListParams{Statuses: []billing.MerchantStatus{billing.MerchantDeleted}})))
	require.Len(t, list(billing.MerchantListParams{Statuses: []billing.MerchantStatus{billing.MerchantActive, billing.MerchantDeleted}}), 2)
	got, err := cp.GetMerchant(ctx, first.MerchantID)
	require.NoError(t, err)
	require.Equal(t, []any{billing.MerchantDeleted, "First", []billing.Rail{}}, []any{got.Status, *got.DisplayName, got.RailsArmed})
	require.NotEqual(t, http.StatusOK, findings(), "a deleted merchant's credentials resolve nothing")

	restored, err := cp.RestoreMerchant(ctx, first.MerchantID)
	require.NoError(t, err)
	require.Equal(t, billing.MerchantActive, restored.Status)
	require.Nil(t, restored.DeletedAt)
	require.Equal(t, http.StatusOK, findings())

	_, err = cp.GetMerchant(ctx, billing.MerchantID(uuid.New()))
	require.ErrorIs(t, err, server.ErrMerchantNotFound)
	require.ErrorIs(t, err, billing.ErrNotFound)

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/v1/platform/merchants"},
		{http.MethodDelete, "/v1/platform/merchants/" + first.MerchantID.String()},
		{http.MethodGet, "/v1/platform/worker-health"},
		{http.MethodGet, "/v1/admin/worker-health"},
		{http.MethodGet, "/metrics"},
	} {
		require.Equal(t, http.StatusNotFound, call(t, handler, key.Secret, route.method, route.path, "", nil).Code, "%s %s", route.method, route.path)
	}

	kind := "e2e_" + uuid.NewString()[:8]
	_, err = f.pool.Exec(ctx, "INSERT INTO "+pgx.Identifier{f.schema, "worker_state"}.Sanitize()+
		" (worker_kind, last_success_at, last_error_at, last_error, consecutive_failures) VALUES ($1, now() - interval '1 hour', now(), 'merchant acme: card declined', 3)", kind)
	require.NoError(t, err)
	health, err := cp.ListWorkerHealth(ctx)
	require.NoError(t, err)
	var row *billing.WorkerHealth
	for i := range health {
		if health[i].WorkerKind == kind {
			row = &health[i]
		}
	}
	require.NotNil(t, row)
	require.Equal(t, int32(3), row.ConsecutiveFailures)
	require.Equal(t, "merchant acme: card declined", *row.LastError, "the operator reads the error verbatim")
	w := httptest.NewRecorder()
	cp.PrivateHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `openrails_worker_consecutive_failures{kind="`+kind+`"} 3`)
	require.Contains(t, w.Body.String(), `openrails_dependency_up{dependency="postgres",class="required"} 1`)

	require.NoError(t, cp.UnlockAdminLockout(ctx, uuid.NewString()))
	require.Error(t, cp.UnlockAdminLockout(ctx, "not-a-user"))
}
