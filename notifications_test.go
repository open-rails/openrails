package openrails

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestListCustomerNotificationsRequest(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	merchant := billing.MerchantID(uuid.New())
	path := "/v1/app/customers/" + customer.String() + "/notifications"
	client, seen := recordingRemote(t, map[string]string{path: `{"data":[{"id":"ntf_77777777-7777-4777-8777-777777777777","merchant_id":"` + merchant.String() + `","customer_id":"` + customer.String() + `","event_type":"future_notice","data":{"message":"New notice"},"seen":false}],"next_cursor":"next"}`})
	unread := false
	page, err := client.ListCustomerNotifications(t.Context(), customer, billing.NotificationListParams{PageRequest: billing.PageRequest{Limit: 12, Cursor: "last"}, Seen: &unread})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, merchant, page.Items[0].MerchantID)
	require.Equal(t, "future_notice", page.Items[0].EventType)
	require.Equal(t, "next", page.Next)
	got := <-seen
	require.Equal(t, http.MethodGet, got.method)
	require.Equal(t, path, got.path)
	require.Equal(t, "cursor=last&limit=12&seen=false", got.query)
	_, err = client.ListCustomerNotifications(t.Context(), billing.CustomerID{}, billing.NotificationListParams{})
	require.Error(t, err)
}
