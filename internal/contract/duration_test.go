package contract

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogDurationAliasesAreDocumented(t *testing.T) {
	files, err := Files(os.DirFS("../.."))
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
				AllOf      []struct {
					Not struct {
						Required []string `json:"required"`
					} `json:"not"`
				} `json:"allOf"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(files[OpenAPIFile], &doc))
	price := doc.Components.Schemas["ApplyPrice"]
	require.NotContains(t, price.Properties, "auto_renew", "renewal choice belongs to the order")
	require.Len(t, price.AllOf, 3)
	for i, name := range []string{"access_duration", "billing_interval", "trial_duration"} {
		require.Contains(t, price.Properties, name)
		require.Contains(t, price.Properties, name+"_hours")
		require.Equal(t, []string{name, name + "_hours"}, price.AllOf[i].Not.Required)
		require.Contains(t, string(files[consoleDir+"wire.ts"]), name+"?: string | null")
	}
}
