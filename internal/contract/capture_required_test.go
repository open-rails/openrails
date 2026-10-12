package contract

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCaptureUsageIsRequiredInTheWireContract(t *testing.T) {
	files, err := Files(os.DirFS("../.."))
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string                  `json:"required"`
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(files[OpenAPIFile], &doc))
	capture := doc.Components.Schemas["CaptureAdmissionParams"]
	require.Equal(t, []string{"usage"}, capture.Required)
	require.Equal(t, map[string]any{"$ref": "#/components/schemas/CaptureUsage"}, capture.Properties["usage"], "required usage is not nullable")
	require.Empty(t, doc.Components.Schemas["RecordUsageParams"].Required, "unannotated inputs do not become blanket requirements")
	require.Contains(t, string(files[consoleDir+"wire.ts"]), "  usage: CaptureUsage\n")
	require.NotContains(t, string(files[consoleDir+"wire.ts"]), "  usage?: CaptureUsage")
}

func TestInputRequiredAnnotationUsesTheExactRule(t *testing.T) {
	type request struct {
		Required    string `json:"required" binding:"required,min=1"`
		Conditional string `json:"conditional" binding:"required_without=Required"`
		Ordinary    string `json:"ordinary"`
	}
	fields := fieldsOf(reflect.TypeFor[request]())
	require.True(t, fields[0].required)
	require.False(t, fields[1].required)
	require.False(t, fields[2].required)
}
