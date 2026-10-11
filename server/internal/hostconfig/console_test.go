package hostconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The host's extension data reaches the console through config.json, keyed by
// extension id.
func TestValidateConsoleExtensions(t *testing.T) {
	for _, row := range []struct {
		extensions map[string]any
		err        string
	}{
		{nil, ""},
		{map[string]any{"hosted": map[string]any{"plans": []any{"starter"}}}, ""},
		{map[string]any{"saas-2": true}, ""},
		{map[string]any{"Hosted": true}, `invalid AdminConsole.Extensions key "Hosted"`},
		{map[string]any{"-x": true}, `invalid AdminConsole.Extensions key "-x"`},
		{map[string]any{"x": func() {}}, "invalid AdminConsole.Extensions.x"},
	} {
		if err := ValidateConsoleExtensions("AdminConsole.Extensions", row.extensions); row.err == "" {
			require.NoError(t, err, row.extensions)
		} else {
			require.ErrorContains(t, err, row.err)
		}
	}
}
