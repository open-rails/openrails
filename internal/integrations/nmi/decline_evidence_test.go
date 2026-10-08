package nmi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefinitiveDeclineRequiresClosedGatewayOutcome(t *testing.T) {
	for _, tc := range []struct {
		condition, success, code string
		closed                   bool
	}{
		{"", "0", "202", true},
		{"failed", "0", "202", true},
		{"complete", "0", "202", true},
		{"failed", "1", "202", false},
		{"failed", "", "202", false},
		{"failed", "unknown", "202", false},
		{"failed", "0", "400", false},
		{"failed", "0", "420", false},
		{"failed", "0", "421", false},
		{"failed", "0", "430", false},
		{"failed", "0", "unknown", false},
		{"unknown", "0", "202", false},
		{"in_progress", "0", "202", false},
		{"pending", "0", "202", false},
		{"pendingsettlement", "0", "202", false},
	} {
		action := QueryAction{Success: tc.success, ResponseCode: tc.code}
		_, closed := action.DefinitiveDecline(tc.condition)
		require.Equal(t, tc.closed, closed, "%+v", tc)
	}
}
