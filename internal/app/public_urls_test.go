package app

import (
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDashboardDestinationIsIndependentOfBillingMount(t *testing.T) {
	cfg := &config.Config{PublicBillingBaseURL: "https://billing.example/billing"}
	require.Empty(t, alertingDashboardBaseURL(cfg))
	cfg.DashboardBaseURL = "https://console.example/admin/"
	require.Equal(t, "https://console.example/admin", alertingDashboardBaseURL(cfg))
}
