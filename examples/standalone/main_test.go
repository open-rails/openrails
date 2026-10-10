package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

// The example's files read as the README describes them: the server is the
// resource at the URL main.go defaults to, and the merchant trusts the app.
func TestExampleFilesRead(t *testing.T) {
	_, err := catalog.ReadFile("catalog.yaml")
	require.NoError(t, err)
	for file, wants := range map[string][]string{
		"openrails/config.yaml":           {"id: http://localhost:3053", "programmatic: true"},
		"openrails/merchant.example.yaml": {"onlydemo:", "issuer: http://localhost:8080"},
	} {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		for _, want := range wants {
			require.True(t, strings.Contains(string(raw), want), "%s: %s", file, want)
		}
	}
}
