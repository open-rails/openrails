//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/server/internal/operator"
)

func TestCustomerNotificationInboxes(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	account := w.account(true)
	customer := uuid.New()
	shopToken := w.customer(w.shopLogin, customer.String(), nil)
	rivalToken := w.customer(w.rivalLogin, customer.String(), nil)
	shopLink := w.connect(shopToken, w.shopClient, account)
	rivalLink := w.connect(rivalToken, w.rivalClient, account)
	graph, _ := operator.Of(w.srv)
	// Identical IDs and timestamps across merchants exercise the complete
	// cross-merchant keyset, and the read mutation's merchant boundary.
	first, second, foreign := uuid.New(), uuid.New(), uuid.New()
	at := time.Now().UTC().Truncate(time.Second)
	for _, mid := range []billing.MerchantID{w.shop, w.rival} {
		mctx := merchant.WithID(ctx, mid)
		for _, id := range []uuid.UUID{first, second} {
			require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationQueue{
				ID: id, CustomerID: customer, EventType: "future_billing_notice", CreatedAt: at,
				Data: billing.NotificationData{Message: "Account notice"},
			}))
		}
		require.NoError(t, graph.Runtime.NotificationService.CreateIfAbsent(mctx, &models.NotificationQueue{
			ID: foreign, CustomerID: uuid.New(), EventType: "other_customer", CreatedAt: at,
		}))
		_, err := graph.Runtime.DB.Gen(mctx).CreateMerchantNotification(mctx, gen.CreateMerchantNotificationParams{
			MerchantID: mid.UUID(), Severity: "high", Title: "Operator only", Body: "Private evidence",
		})
		require.NoError(t, err)
	}
	appPath := "/v1/app/customers/" + customer.String() + "/notifications"
	r := call(t, w.handler, w.shopKey.Secret, http.MethodGet, appPath+"?limit=1", nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	page := decode[billing.ListPage[billing.Notification]](t, r)
	require.Len(t, page.Items, 1)
	require.Equal(t, w.shop, page.Items[0].MerchantID)
	require.Equal(t, "future_billing_notice", page.Items[0].EventType)
	require.NotEmpty(t, page.Next)
	r = call(t, w.handler, w.shopKey.Secret, http.MethodGet, appPath+"?limit=1&cursor="+url.QueryEscape(page.Next), nil)
	last := decode[billing.ListPage[billing.Notification]](t, r)
	require.Len(t, last.Items, 1)
	require.NotEqual(t, page.Items[0].ID, last.Items[0].ID)
	require.Empty(t, last.Next)
	w.refused(serve(w.handler, rsRequest{method: http.MethodGet, path: appPath, authorization: shopToken}), http.StatusForbidden, billing.CodeApplicationRequired, "a customer cannot read the host feed")
	w.refused(call(t, w.handler, w.shopKey.Secret, http.MethodGet, appPath+"?cursor=invalid", nil), http.StatusBadRequest, billing.CodeInvalidCursor, "invalid cursor")

	session := "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), account).AccessToken
	w.refused(w.network(http.MethodGet, "/notifications", "", nil), http.StatusUnauthorized, billing.CodeAuthenticationRequired, "no account")
	other := "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), w.account(true)).AccessToken
	require.Empty(t, networkList[billing.NetworkNotification](w, "/notifications", other, 1))
	all := networkList[billing.NetworkNotification](w, "/notifications", session, 50)
	paged := networkList[billing.NetworkNotification](w, "/notifications", session, 1)
	require.Equal(t, all, paged)
	require.Len(t, all, 4, "only linked customer notices, not operator or other-customer messages")
	for _, item := range all {
		require.Equal(t, item.MerchantID, item.Notification.MerchantID)
		if item.MerchantID == w.shop {
			require.Equal(t, "Network Shop", item.Merchant.DisplayName)
		}
		require.Equal(t, billing.CustomerID(customer), item.Notification.CustomerID)
	}
	readPath := "/links/" + shopLink.ID.String() + "/notifications/read"
	w.refused(w.network(http.MethodPost, readPath, other, billing.MarkNotificationsReadParams{All: true}), http.StatusNotFound, billing.CodeResourceNotFound, "another account's link")
	for range 2 {
		r = w.network(http.MethodPost, readPath, session, billing.MarkNotificationsReadParams{NotificationIDs: []billing.NotificationID{billing.NotificationID(first), billing.NotificationID(foreign)}})
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		marked := decode[billing.CustomerNotificationLookup](t, r)
		require.True(t, marked.Notifications[billing.NotificationID(first)].Seen)
		require.Nil(t, marked.Notifications[billing.NotificationID(foreign)])
	}
	r = w.me(http.MethodGet, "/notifications?seen=true", shopToken, nil)
	read := decode[billing.ListPage[billing.Notification]](t, r)
	require.Len(t, read.Items, 1, "network and merchant share read state")
	r = call(t, w.handler, w.rivalKey.Secret, http.MethodGet, appPath+"?seen=true", nil)
	require.Empty(t, decode[billing.ListPage[billing.Notification]](t, r).Items, "same notice ID at another merchant remains unread")
	r = w.network(http.MethodDelete, "/links/"+rivalLink.ID.String(), session, nil)
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())
	require.Len(t, networkList[billing.NetworkNotification](w, "/notifications", session, 1), 2, "revocation hides the merchant immediately")
	w.refused(w.network(http.MethodPost, "/links/"+rivalLink.ID.String()+"/notifications/read", session, billing.MarkNotificationsReadParams{All: true}), http.StatusNotFound, billing.CodeResourceNotFound, "revoked link")
}
