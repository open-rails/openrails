//go:build e2e && integration

package ci_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/schemasnapshot"
)

// TestSchemaSnapshot keeps api/schema.txt equal to the schema the migrations
// install, so every schema change is deliberate: regenerate with
// go run ./scripts/contracts -write and review the diff.
func TestSchemaSnapshot(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv(schemasnapshot.DSNEnv))
	require.NotEmpty(t, dsn, schemasnapshot.DSNEnv+" must point at a disposable PostgreSQL server")
	listed, err := os.ReadFile("../" + schemasnapshot.File)
	require.NoError(t, err)
	applied, err := schemasnapshot.Capture(t.Context(), dsn)
	require.NoError(t, err)

	gone, added := schemasnapshot.Diff(listed, applied)
	if len(gone) > 0 {
		t.Errorf("removed or changed (%v):\n%s", schemasnapshot.ErrStale, strings.Join(gone, "\n"))
	}
	if len(added) > 0 {
		t.Errorf("added (%v):\n%s", schemasnapshot.ErrStale, strings.Join(added, "\n"))
	}
	if string(listed) != string(applied) {
		t.Error(schemasnapshot.ErrStale)
	}
}
