//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/openrails/billing"
	engineauthtest "github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/stretchr/testify/require"
)

// Every authorized reader owns its cursor. Reading or rotating a key does not
// register a consumer or remove facts another application still needs.
func TestNotificationEventLogReaders(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	graph, _ := operator.Of(w.srv)
	database := graph.Runtime.DB
	path := "/v1/app/events?type=order_due"
	read := func(h http.Handler, key, cursor string) billing.EventPage {
		t.Helper()
		query := path
		if cursor != "" {
			query += "&after=" + url.QueryEscape(cursor)
		}
		r := call(t, h, key, http.MethodGet, query, nil)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		return decode[billing.EventPage](t, r)
	}
	before := read(w.handler, w.shopKey.Secret, "")
	require.Empty(t, before.Events)
	require.NotEmpty(t, before.NextCursor)

	customer, id := uuid.New(), uuid.New()
	at := time.Now().UTC().Add(-time.Hour)
	for _, mid := range []billing.MerchantID{w.shop, w.rival} {
		mctx := merchant.WithID(ctx, mid)
		require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationEvent{ID: id, CustomerID: customer, EventType: models.NotificationOrderDue, CreatedAt: at, Data: billing.NotificationData{Message: "invoice due"}}))
		require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationEvent{ID: id, CustomerID: customer, EventType: models.NotificationOrderDue, CreatedAt: at}))
	}
	mine := read(w.handler, w.shopKey.Secret, before.NextCursor)
	require.Len(t, mine.Events, 1)
	require.Equal(t, w.shop, mine.Events[0].MerchantID)
	require.Equal(t, "notification", mine.Events[0].Source)
	require.Equal(t, "order_due", mine.Events[0].Type)
	rotated := merchantKey(t, w.srv, w.shop, "owner")
	require.Equal(t, mine, read(w.handler, rotated.Secret, before.NextCursor), "key rotation preserves the merchant stream")
	require.Empty(t, read(w.handler, rotated.Secret, mine.NextCursor).Events)
	require.Len(t, read(w.handler, w.rivalKey.Secret, "").Events, 1, "same source ID in another merchant has its own stream")
	foreign := call(t, w.handler, w.rivalKey.Secret, http.MethodGet, path+"&after="+url.QueryEscape(mine.NextCursor), nil)
	require.Equal(t, http.StatusBadRequest, foreign.Code, foreign.Body.String())

	// Application identity does not create a source-owned subscription. Two
	// authorized applications replay the same immutable facts independently.
	fake := &engineauthtest.Fake{}
	mux := http.NewServeMux()
	routes.RegisterAppRoutes(router.NewMux(mux, "/v1", graph.Runtime), graph.Runtime, routes.Options{Auth: fake, Groups: config.RouteGroups{Programmatic: true}})
	scope := engineauthtest.MerchantScope(w.shop)
	tokenOf := func(id string) string {
		return fake.Issue(engineauthtest.Grant{Identity: engineauthtest.Application(id), Permissions: []billing.Permission{billing.MerchantEventsRead}, Scope: scope, Bound: scope})
	}
	appA, appB := tokenOf("app-a"), tokenOf("app-b")
	require.Equal(t, mine, read(mux, appA, before.NextCursor))
	require.Equal(t, mine, read(mux, appB, before.NextCursor))
	global, err := w.srv.ListEvents(ctx, w.shop, billing.EventQuery{Types: []string{"order_due"}})
	require.NoError(t, err)
	decodeJSON := func(value billing.EventPage) any {
		t.Helper()
		value.Events = append([]billing.Event(nil), value.Events...)
		for i := range value.Events {
			value.Events[i].OccurredAt = value.Events[i].OccurredAt.UTC()
		}
		raw, err := json.Marshal(value)
		require.NoError(t, err)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var decoded any
		require.NoError(t, decoder.Decode(&decoded))
		return decoded
	}
	require.Equal(t, decodeJSON(mine), decodeJSON(*global), "privileged readers use the same merchant facts")

	// A fact written by a transaction that commits after a reader advances is
	// still returned, even when its domain occurrence time is older.
	tx, err := database.DataPool().Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	late := uuid.New()
	require.NoError(t, gen.New(tx).CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{ID: late, MerchantID: w.shop.UUID(), CustomerID: customer, EventType: "future_kind", Data: []byte(`{"downgrade_applied":"future-kind-value","future":{"exact":"9007199254740993"}}`), CreatedAt: at.Add(-time.Hour)}))
	allBefore, err := w.srv.ListEvents(ctx, w.shop, billing.EventQuery{})
	require.NoError(t, err)
	require.Len(t, allBefore.Events, 1, "uncommitted facts are invisible")
	require.NoError(t, tx.Commit(ctx))
	latePage, err := w.srv.ListEvents(ctx, w.shop, billing.EventQuery{After: allBefore.NextCursor})
	require.NoError(t, err)
	require.Len(t, latePage.Events, 1)
	require.Equal(t, "ntf_"+late.String(), latePage.Events[0].ID)
	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(latePage.Events[0].Data, &data))
	require.JSONEq(t, `{"exact":"9007199254740993"}`, string(data["future"]))
	require.Equal(t, `"future-kind-value"`, string(data["downgrade_applied"]))
	require.Equal(t, mine.Events, read(w.handler, w.shopKey.Secret, before.NextCursor).Events, "another reader cannot consume history")

	token := w.customer(w.shopLogin, customer.String(), nil)
	require.Equal(t, http.StatusNotFound, w.me(http.MethodGet, "/notifications", token, nil).Code)
	denied := serve(w.handler, rsRequest{method: http.MethodGet, path: path, authorization: token})
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	for _, oldPath := range []string{"/v1/app/notification-consumers/contentkit", "/v1/app/notification-consumers/contentkit/events", "/v1/app/host-events", "/v1/app/customers/" + customer.String() + "/notifications"} {
		r := call(t, w.handler, w.shopKey.Secret, http.MethodGet, oldPath, nil)
		require.Equal(t, http.StatusNotFound, r.Code, r.Body.String())
	}
}

func TestHostNetworkAccountAdmission(t *testing.T) {
	w := newWorld(t)
	accepted := w.account(true)
	noTerms := w.account(false)
	handler := w.srv.NetworkAccountHandler(func(rw http.ResponseWriter, r *http.Request, account billing.NetworkAccountID) {
		links, err := w.srv.NetworkLinks(r.Context(), account)
		require.NoError(t, err)
		require.Equal(t, accepted.ID, account.String())
		require.Empty(t, links)
		rw.WriteHeader(http.StatusNoContent)
	})
	for _, tc := range []struct {
		token  string
		status int
	}{
		{"", 401},
		{"Bearer " + w.shopKey.Secret, 403},
		{w.customer(w.shopLogin, uuid.NewString(), nil), 403}, {"Bearer " + authtest.SignIn(t, w.srv.AuthKit(), noTerms).AccessToken, 403},
		{"Bearer " + authtest.SignIn(t, w.srv.AuthKit(), accepted).AccessToken, 204},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/notifications", nil)
		req.Header.Set("Authorization", tc.token)
		rw := httptest.NewRecorder()
		handler.ServeHTTP(rw, req)
		require.Equal(t, tc.status, rw.Code, rw.Body.String())
	}
}
