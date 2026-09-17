// currency-units generates the admin UI scale table from the public registry.
package main

import (
	"encoding/json"
	"os"

	"github.com/open-rails/openrails"
)

func main() {
	units := map[string]int{}
	for _, currency := range openrails.Currencies() {
		units[currency.Code] = currency.Decimals
	}
	raw, err := json.MarshalIndent(units, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("web/admin/src/lib/currency-units.json", append(raw, '\n'), 0644); err != nil {
		panic(err)
	}
}
