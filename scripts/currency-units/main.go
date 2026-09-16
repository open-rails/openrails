// currency-units generates the admin UI scale table from the engine registry.
package main

import (
	"encoding/json"
	"os"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func main() {
	units := map[string]int{}
	for _, code := range moneyutil.CurrencyCodes() {
		units[code], _ = moneyutil.CurrencyScale(code)
	}
	raw, err := json.MarshalIndent(units, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("web/admin/src/lib/currency-units.json", append(raw, '\n'), 0644); err != nil {
		panic(err)
	}
}
