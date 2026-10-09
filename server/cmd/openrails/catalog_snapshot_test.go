package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogSnapshotCLIRequiresExplicitIdentityAndFiles(t *testing.T) {
	for _, verb := range []string{"export", "import"} {
		_, err := execute(newCatalogCmd(), verb, "--merchant", "my-app")
		require.ErrorContains(t, err, "exact UUID")
	}
	const merchant = "10000000-0000-0000-0000-000000000001"
	_, err := execute(newCatalogCmd(), "export", "--merchant", merchant)
	require.ErrorContains(t, err, "--out")
	_, err = execute(newCatalogCmd(), "import", "--merchant", merchant)
	require.ErrorContains(t, err, "--in")
	_, err = execute(newCatalogCmd(), "import", "--merchant", merchant, "--overwrite")
	require.ErrorContains(t, err, "unknown flag")
	for _, verb := range []string{"export", "import"} {
		help, err := execute(newRootCmd(), "catalog", verb, "--help")
		require.NoError(t, err)
		require.Contains(t, help, "--merchant")
		require.Contains(t, help, "UUID")
	}
}
