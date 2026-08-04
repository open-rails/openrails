package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRLSPostureError pins the startup gate (or#782): a role that bypasses RLS
// is refused UNCONDITIONALLY. There is no environment argument and no
// development exemption — that exemption is what let missing-merchant-scope
// bugs look healthy on a laptop and run inert in production.
func TestRLSPostureError(t *testing.T) {
	require.NoError(t, rlsPostureError(RLSPosture{CurrentUser: "openrails_app", Enforcing: true}))

	err := rlsPostureError(RLSPosture{CurrentUser: "postgres", Enforcing: false})
	require.Error(t, err)
	require.Contains(t, err.Error(), "postgres")
	require.Contains(t, err.Error(), "openrails_app")
	require.Contains(t, err.Error(), "development")
}
