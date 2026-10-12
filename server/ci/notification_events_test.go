//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/notificationevents"
	"github.com/open-rails/openrails/server/internal/operator"
	"github.com/stretchr/testify/require"
)

func TestNotificationEventConsumers(t *testing.T) {
	call := func(t *testing.T, h http.Handler, key, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		payload := ""
		if body != nil {
			b, err := json.Marshal(body)
			require.NoError(t, err)
			payload = string(b)
		}
		return serve(h, rsRequest{method: method, path: path, authorization: "Bearer " + key, body: payload, idempotencyKey: uuid.NewString()})
	}
	w := newWorld(t)
	ctx := t.Context()
	graph, _ := operator.Of(w.srv)
	database := graph.Runtime.DB
	global, err := w.srv.NotificationEvents("saas")
	require.NoError(t, err)
	require.NoError(t, global.Register(ctx))
	path := "/v1/app/notification-consumers/contentkit"
	for _, key := range []string{w.shopKey.Secret, w.rivalKey.Secret} {
		r := call(t, w.handler, key, http.MethodPost, path, nil)
		require.Equal(t, 204, r.Code, r.Body.String())
	}
	rotated := merchantKey(t, w.srv, w.shop, "owner")
	r := call(t, w.handler, rotated.Secret, http.MethodGet, path+"/events", nil)
	require.Equal(t, 200, r.Code, "key rotation preserves the verified application subject: %s", r.Body.String())
	customer, id := uuid.New(), uuid.New()
	at := time.Now().UTC().Add(-time.Hour)
	for _, mid := range []billing.MerchantID{w.shop, w.rival} {
		mctx := merchant.WithID(ctx, mid)
		require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationEvent{ID: id, CustomerID: customer, EventType: models.NotificationOrderDue, CreatedAt: at, Data: billing.NotificationData{Message: "invoice due"}}))
		require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationEvent{ID: id, CustomerID: customer, EventType: models.NotificationOrderDue, CreatedAt: at}))
	}
	pending := func(key string) []billing.NotificationEvent {
		t.Helper()
		r := call(t, w.handler, key, http.MethodGet, path+"/events?limit=100", nil)
		require.Equal(t, 200, r.Code, r.Body.String())
		return decode[billing.NotificationEvents](t, r).Data
	}
	mine := pending(w.shopKey.Secret)
	require.Len(t, mine, 1)
	require.Equal(t, w.shop, mine[0].MerchantID)
	require.True(t, mine[0].ActionRequested)
	// Independent verified application subjects in the same merchant cannot
	// consume or retire each other's same-named subscriptions.
	fake := &engineauthtest.Fake{}
	mux := http.NewServeMux()
	routes.RegisterAppRoutes(router.NewMux(mux, "/v1", graph.Runtime), graph.Runtime, routes.Options{Auth: fake, Groups: config.RouteGroups{Programmatic: true}})
	scope := engineauthtest.MerchantScope(w.shop)
	tokenOf := func(id string) string {
		return fake.Issue(engineauthtest.Grant{Identity: engineauthtest.Application(id), Permissions: []billing.Permission{billing.MerchantEventsRead}, Scope: scope, Bound: scope})
	}
	appA, appB := tokenOf("app-a"), tokenOf("app-b")
	isolated := "/v1/app/notification-consumers/same-name"
	response := call(t, mux, appA, http.MethodPost, isolated, nil)
	require.Equal(t, 204, response.Code, response.Body.String())
	response = call(t, mux, appB, http.MethodGet, isolated+"/events", nil)
	require.Equal(t, 404, response.Code, response.Body.String())
	response = call(t, mux, appB, http.MethodDelete, isolated, nil)
	require.Equal(t, 204, response.Code, response.Body.String())
	response = call(t, mux, appA, http.MethodGet, isolated+"/events", nil)
	require.Equal(t, 200, response.Code, response.Body.String())
	response = call(t, mux, appB, http.MethodPost, isolated, nil)
	require.Equal(t, 204, response.Code, response.Body.String())
	response = call(t, mux, appA, http.MethodPost, isolated+"/acknowledge", billing.AcknowledgeNotificationEventsParams{NotificationIDs: []billing.NotificationID{billing.NotificationID(id)}})
	require.Equal(t, 204, response.Code, response.Body.String())
	response = call(t, mux, appB, http.MethodGet, isolated+"/events", nil)
	require.Len(t, decode[billing.NotificationEvents](t, response).Data, 1)
	for _, key := range []string{appA, appB} {
		response = call(t, mux, key, http.MethodDelete, isolated, nil)
		require.Equal(t, 204, response.Code, response.Body.String())
	}
	all, err := global.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, all, 2)
	refs := []billing.NotificationEventRef{{MerchantID: w.shop, ID: billing.NotificationID(id)}}
	require.NoError(t, global.Acknowledge(ctx, refs))
	require.Len(t, pending(w.shopKey.Secret), 1, "global ACK cannot consume remote delivery")
	r = call(t, w.handler, rotated.Secret, http.MethodPost, path+"/acknowledge", billing.AcknowledgeNotificationEventsParams{NotificationIDs: []billing.NotificationID{billing.NotificationID(id)}})
	require.Equal(t, 204, r.Code, r.Body.String())
	require.Empty(t, pending(w.shopKey.Secret))
	require.Len(t, pending(w.rivalKey.Secret), 1, "same source id at another merchant remains pending")
	all, err = global.Pending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, w.rival, all[0].MerchantID)
	r = call(t, w.handler, w.shopKey.Secret, http.MethodDelete, path, nil)
	require.Equal(t, 204, r.Code, r.Body.String())
	r = call(t, w.handler, w.shopKey.Secret, http.MethodPost, path, nil)
	require.Equal(t, 204, r.Code, r.Body.String())
	require.Len(t, pending(w.shopKey.Secret), 1, "re-registration replays retained facts")
	tx, err := database.DataPool().Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	late := uuid.New()
	require.NoError(t, gen.New(tx).CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{ID: late, MerchantID: w.shop.UUID(), CustomerID: customer, EventType: "future_kind", Data: []byte(`{"downgrade_applied":"future-kind-value","future":{"exact":"9007199254740993"}}`), CreatedAt: at.Add(-time.Hour)}))
	require.Len(t, pending(w.shopKey.Secret), 1, "uncommitted facts are invisible")
	r = call(t, w.handler, w.shopKey.Secret, http.MethodPost, path+"/acknowledge", billing.AcknowledgeNotificationEventsParams{NotificationIDs: []billing.NotificationID{billing.NotificationID(id)}})
	require.Equal(t, 204, r.Code, r.Body.String())
	require.NoError(t, tx.Commit(ctx))
	mine = pending(w.shopKey.Secret)
	require.Len(t, mine, 1)
	require.Equal(t, billing.NotificationID(late), mine[0].ID)
	require.JSONEq(t, `{"downgrade_applied":"future-kind-value","future":{"exact":"9007199254740993"}}`, string(mine[0].Data))
	_, err = notificationevents.Prune(ctx, database, time.Now().Add(366*24*time.Hour), 1000)
	require.NoError(t, err)
	require.Len(t, pending(w.shopKey.Secret), 1, "pending deliveries never expire")
	for _, key := range []string{w.shopKey.Secret, w.rivalKey.Secret} {
		r := call(t, w.handler, key, http.MethodDelete, path, nil)
		require.Equal(t, 204, r.Code, r.Body.String())
	}
	err = merchantarchive.Export(ctx, database, w.shop, io.Discard)
	var refused *merchantarchive.Error
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "notification_events", refused.Table, "global consumer blocks archive too")
	token := w.customer(w.shopLogin, customer.String(), nil)
	require.Equal(t, 404, w.me(http.MethodGet, "/notifications", token, nil).Code)
	denied := serve(w.handler, rsRequest{method: http.MethodGet, path: path + "/events", authorization: token})
	require.Equal(t, 403, denied.Code, denied.Body.String())
	r = call(t, w.handler, w.shopKey.Secret, http.MethodGet, "/v1/app/customers/"+customer.String()+"/notifications", nil)
	require.Equal(t, 404, r.Code)
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
