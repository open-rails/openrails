package openrails

import (
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestListEventsRequest(t *testing.T) {
	client, requests := recordingRemote(t, map[string]string{"/v1/app/events": `{"events":[],"next_cursor":"next","has_more":false,"retention_seconds":7776000}`})
	page, err := client.ListEvents(t.Context(), billing.EventQuery{After: "cursor+/?", Limit: 25, Types: []string{"premium_ended", "payment_failed"}})
	require.NoError(t, err)
	require.NotNil(t, page.Events)
	require.Equal(t, "next", page.NextCursor)
	request := <-requests
	require.Equal(t, "GET", request.method)
	require.Equal(t, "/v1/app/events", request.path)
	require.Equal(t, "after=cursor%2B%2F%3F&limit=25&type=premium_ended&type=payment_failed", request.query)
	require.Nil(t, request.body)
}
