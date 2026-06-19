package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRLSPostureError pins the startup gate decision (issue #227): non-dev
// environments (require=true, derived from env) MUST fail fast if the connected
// role bypasses RLS, while development (require=false) only warns.
func TestRLSPostureError(t *testing.T) {
	cases := []struct {
		name      string
		posture   RLSPosture
		require   bool
		wantError bool
	}{
		{"enforcing role, required", RLSPosture{CurrentUser: "openrails_app", Enforcing: true}, true, false},
		{"enforcing role, not required", RLSPosture{CurrentUser: "openrails_app", Enforcing: true}, false, false},
		{"bypass role, non-dev => FAIL", RLSPosture{CurrentUser: "postgres", Enforcing: false}, true, true},
		{"bypass role, dev => warn only", RLSPosture{CurrentUser: "postgres", Enforcing: false}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rlsPostureError(tc.posture, tc.require)
			if tc.wantError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.posture.CurrentUser)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
