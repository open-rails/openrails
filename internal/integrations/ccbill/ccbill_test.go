package ccbill

import (
	"net/url"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestGenerateFlexFormURLIncludesReservationID(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "123", ClientSubAcc: "456"}, true)

	resp, err := client.GenerateFlexFormURL(&GenerateFlexFormURLParams{
		Username:      "user_123",
		Email:         "user@example.com",
		FormName:      "form-name",
		FlexID:        "flex-123",
		ReservationID: "cs_11111111-1111-1111-1111-111111111111",
	})

	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "cs_11111111-1111-1111-1111-111111111111", parsed.Query().Get("reservationId"))
}
