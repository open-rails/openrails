package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/catalog"
)

// The catalog the app applies at boot reads.
func TestExampleFilesRead(t *testing.T) {
	_, err := catalog.ReadFile("catalog.yaml")
	require.NoError(t, err)
}
