package nmi

import (
	"context"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCheckTestModeArmRequiresFreshQualification(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		allowed        bool
	}{
		{"simulated", "1", true}, {"live", "2", false}, {"indeterminate", "3", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := probeServer(t, tc.response, nil)
			defer server.Close()
			client := probeClient(t, server.URL)
			client.endpointDeployment = "sandbox"
			err := CheckTestModeArm(context.Background(), client)
			if tc.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "refusing to arm")
			}
			server.Close()
			require.ErrorContains(t, CheckTestModeArm(context.Background(), client), "refusing to arm", "a previous result must not authorize an account without a fresh probe")
		})
	}
}
