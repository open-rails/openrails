package contract

import "testing"

func TestPriceBindingConfigurationArchive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"empty", `{}`, true},
		{"stripe", `{"product_id":"prod_premium","lookup_key":"premium-monthly"}`, true},
		{"nmi", `{"provider":"mobius"}`, true},
		{"ccbill", `{"form_name":"premium"}`, true},
		{"solana transfer", `{"provider":"solana","enabled":"true"}`, true},
		{"solana plan", `{"token":"USDC","mint":"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v","mint_symbol":"USDC","amount_base_units":"9990000","period_hours":"720","created_at":"1700000000","merchant_address":"11111111111111111111111111111111"}`, true},
		{"unknown field", `{"future_config":"value"}`, false},
		{"secret field", `{"api_key":"secret"}`, false},
		{"secret in public field", `{"token":"sk_live_example"}`, false},
		{"card in public field", `{"form_name":"4111111111111111"}`, false},
		{"identity in config", `{"price_id":"price_1"}`, false},
		{"number instead of string", `{"amount_base_units":9990000}`, false},
		{"boolean instead of string", `{"enabled":true}`, false},
		{"nested public field", `{"provider":{"api_key":"secret"}}`, false},
		{"duplicate field", `{"form_name":"premium","form_name":"other"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, values := row(t, "price_psp_bindings", map[string]string{"configuration": tc.value})
			if err := ValidateValues(p, values); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
