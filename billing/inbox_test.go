package billing

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/notify"
	"github.com/stretchr/testify/require"
)

func TestInboxProjectionsKeepAudienceWorkflowAndReadState(t *testing.T) {
	at := time.Now().UTC()
	read := at.Add(time.Minute)
	mid := MerchantID(uuid.New())
	n := Notification{ID: NotificationID(uuid.New()), MerchantID: mid, CustomerID: CustomerID(uuid.New()),
		EventType: "payment_method_failed", CreatedAt: at, ReadAt: &read, Seen: true, ActionRequested: true}
	item, err := n.InboxItem("https://billing.example")
	require.NoError(t, err)
	require.Equal(t, notify.AudienceUser, item.Recipient.Audience)
	require.Equal(t, n.CustomerID.String(), item.Recipient.ID)
	require.Equal(t, notify.Origin{System: "https://billing.example", Scope: mid.String(), ID: n.ID.String()}, item.Origin)
	require.Equal(t, notify.StatusNotice, item.Status, "a historical failure does not claim the account still needs repair")
	require.True(t, item.ActionRequested)
	require.True(t, CustomerNotificationActionRequested(n.EventType))
	require.Equal(t, &read, item.ReadAt)
	require.Nil(t, item.ResolvedAt)
	require.Zero(t, item.Revision)
	n.EventType = "future_notice"
	general, err := n.InboxItem("https://billing.example")
	require.NoError(t, err)
	require.Equal(t, notify.StatusNotice, general.Status)
	_, err = n.InboxItem("")
	require.ErrorIs(t, err, notify.ErrInvalid)

	f := Finding{ID: FindingID(uuid.New()), MerchantID: mid, Type: "life.payment.failed", Status: "requires_review",
		Severity: "high", CreatedAt: at, UpdatedAt: read, Evidence: map[string]any{"private": "operator only"}}
	for status, expected := range map[FindingStatus]notify.Status{"requires_review": notify.StatusOpen, "fixed": notify.StatusResolved, "ignored": notify.StatusDismissed} {
		f.Status = status
		f.ResolvedAt = nil
		if status != "requires_review" {
			f.ResolvedAt = &read
		}
		operator, err := f.InboxItem("https://billing.example")
		require.NoError(t, err)
		require.Equal(t, notify.AudienceOperator, operator.Recipient.Audience)
		require.Equal(t, mid.String(), operator.Recipient.ID)
		require.Equal(t, expected, operator.Status)
		require.True(t, operator.ActionRequested)
		require.Nil(t, operator.ReadAt, "findings have no per-person read marker")
		var payload Finding
		require.NoError(t, json.Unmarshal(operator.Data, &payload))
		require.Equal(t, f.Evidence, payload.Evidence)
	}
}
