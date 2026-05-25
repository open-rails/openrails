package app

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestCreateCCBillDataLinkClientPropagatesTestMode(t *testing.T) {
	t.Parallel()

	testMode := true
	cfg := config.GetDefaultBillingConfig()
	cfg.TestMode = &testMode
	cfg.Processors = map[string]*config.ProcessorConfig{
		"ccbill": {
			ClientAccNum:     "945280",
			ClientSubAcc:     "0001",
			DataLinkUsername: "datalink-user",
			DataLinkPassword: "datalink-pass",
		},
	}

	client := createCCBillDataLinkClient(cfg)
	require.NotNil(t, client)
	require.True(t, client.DevMode)
}
