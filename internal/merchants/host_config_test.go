package merchants

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #850: api_host is operator-supplied (manifest key + merchant-admin route), so
// the format wall lives in ValidateAPIHost and is enforced inside SetHostConfig.
func TestValidateAPIHost(t *testing.T) {
	valid := []string{
		"api.myapp.example",
		"api.host-late.test",
		"localhost",
		"a1.b2.c3",
	}
	for _, host := range valid {
		require.NoError(t, ValidateAPIHost(host), host)
	}
	invalid := []string{
		"",
		"https://api.myapp.example",
		"api.myapp.example/path",
		"api my app",
		"API.Upper.Case", // ValidateAPIHost takes NORMALIZED input
		".leading.dot",
		"double..dot",
		"-leading.hyphen",
		"trailing-.hyphen",
		"under_score.example",
	}
	for _, host := range invalid {
		require.ErrorIs(t, ValidateAPIHost(host), ErrInvalidAPIHost, host)
	}
}

func TestNormalizeAPIHostStripsPortAndCase(t *testing.T) {
	require.Equal(t, "api.myapp.example", NormalizeAPIHost("  API.MyApp.Example:8443 "))
	require.Equal(t, "", NormalizeAPIHost("   "))
	// Normalize then validate is the operator-input pipeline.
	require.NoError(t, ValidateAPIHost(NormalizeAPIHost("API.MyApp.Example:8443")))
}
