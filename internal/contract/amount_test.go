package contract

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogHumanAmountInputIsDocumented(t *testing.T) {
	files, err := Files(os.DirFS("../.."))
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Type        string   `json:"type"`
					Description string   `json:"description"`
					Examples    []string `json:"examples"`
				} `json:"properties"`
				AllOf []struct {
					Not struct {
						Required []string `json:"required"`
					} `json:"not"`
				} `json:"allOf"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(files[OpenAPIFile], &doc))
	price := doc.Components.Schemas["ApplyPrice"]
	amount := price.Properties["amount"]
	require.Equal(t, "string", amount.Type, "amount is a non-nullable input string")
	require.Contains(t, amount.Description, "required registered currency")
	require.Equal(t, []string{"9.99 USD", "1 SOL", "10 USDC"}, amount.Examples)
	var exclusions [][]string
	for _, condition := range price.AllOf {
		exclusions = append(exclusions, condition.Not.Required)
	}
	require.Contains(t, exclusions, []string{"amount", "unit_amount"})
	require.Contains(t, exclusions, []string{"amount", "currency"})
	require.Contains(t, string(files[consoleDir+"wire.ts"]), "amount?: string\n")
	require.NotContains(t, price.Properties, "trial_amount")
}
