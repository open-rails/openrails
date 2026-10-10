package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

// The example's files read as the README describes them, and the server's
// config trusts the app at the URLs main.go defaults to.
func TestExampleFilesRead(t *testing.T) {
	_, err := catalog.ReadFile("catalog.yaml")
	require.NoError(t, err)
	config, err := os.ReadFile("openrails/config.yaml")
	require.NoError(t, err)
	for _, want := range []string{"identifier: http://localhost:3053", "issuer: http://localhost:8080", "merchants: [onlydemo]", "programmatic: true"} {
		require.True(t, strings.Contains(string(config), want), "openrails/config.yaml: %s", want)
	}
}
