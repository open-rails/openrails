// Package examples holds no code: each of embedded, standalone and hosted
// is a complete app of its own. This test keeps the parts they share
// identical, so they differ only where their mode does.
package examples

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSharedFilesAreIdentical(t *testing.T) {
	shared := []string{
		"catalog.yaml", "content.go", "media/courses/css-101.mp4", "media/courses/tailwind-102.mp4", "media/courses/live-qa.mp4",
		"web/index.html", "web/src/main.tsx", "web/src/pages.tsx", "web/e2e/gated.spec.ts",
		"web/playwright.config.ts", "web/vite.config.ts", "web/tsconfig.json", "web/pnpm-lock.yaml", "web/pnpm-workspace.yaml",
	}
	for _, name := range shared {
		want, err := os.ReadFile(filepath.Join("embedded", name))
		require.NoError(t, err)
		for _, mode := range []string{"standalone", "hosted"} {
			got, err := os.ReadFile(filepath.Join(mode, name))
			require.NoError(t, err)
			require.Equal(t, string(want), string(got), "%s/%s differs from embedded/%s", mode, name, name)
		}
	}
}
