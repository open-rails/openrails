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
		Currency:      "USD",
		ReservationID: "cs_11111111-1111-1111-1111-111111111111",
	})

	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "cs_11111111-1111-1111-1111-111111111111", parsed.Query().Get("reservationId"))
}

func TestGenerateFlexFormURLOmitsEmptyOptionalAddressFields(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "123", ClientSubAcc: "456"}, true)

	resp, err := client.GenerateFlexFormURL(&GenerateFlexFormURLParams{
		Username:      "user_123",
		Email:         "user@example.com",
		CustomerFName: "User",
		CustomerLName: "Example",
		ZipCode:       "10001",
		Country:       "US",
		FormName:      "form-name",
		FlexID:        "flex-123",
		Currency:      "USD",
	})

	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	query := parsed.Query()
	require.False(t, query.Has("address1"))
	require.False(t, query.Has("city"))
	require.False(t, query.Has("state"))
	require.Equal(t, "10001", query.Get("zipcode"))
	require.Equal(t, "US", query.Get("country"))
}

func TestGenerateFlexFormURLKeepsRealOptionalAddressFields(t *testing.T) {
	client := NewClient(&config.CCBillConfig{ClientAccNum: "123", ClientSubAcc: "456"}, true)

	resp, err := client.GenerateFlexFormURL(&GenerateFlexFormURLParams{
		Username:      "user_123",
		Email:         "user@example.com",
		CustomerFName: "User",
		CustomerLName: "Example",
		Address1:      " 1 Main St ",
		City:          " New York ",
		State:         " NY ",
		ZipCode:       "10001",
		Country:       "US",
		FormName:      "form-name",
		FlexID:        "flex-123",
		Currency:      "USD",
	})

	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	query := parsed.Query()
	require.Equal(t, "1 Main St", query.Get("address1"))
	require.Equal(t, "New York", query.Get("city"))
	require.Equal(t, "NY", query.Get("state"))
}
