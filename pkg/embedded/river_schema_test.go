package embedded

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestResolveHostRiverSchema(t *testing.T) {
	cases := []struct {
		name          string
		clientSchema  string
		billingSchema string
		want          string
		wantErr       bool
	}{
		{"empty adopts the default", "", "openrails", config.RiverSchema, false},
		{"whitespace adopts the default", "  ", "openrails", config.RiverSchema, false},
		{"public stays public", "public", "openrails", "public", false},
		{"host-owned schema is adopted", "doujins_river", "openrails", "doujins_river", false},
		{"billing schema is refused", "openrails", "openrails", "", true},
		{"custom billing schema is refused", "acme_billing", "acme_billing", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveHostRiverSchema(tc.clientSchema, tc.billingSchema)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveHostRiverSchema(%q, %q) accepted the billing schema for river state", tc.clientSchema, tc.billingSchema)
				}
				if !strings.Contains(err.Error(), tc.billingSchema) {
					t.Fatalf("refusal does not name the offending schema: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveHostRiverSchema(%q, %q): %v", tc.clientSchema, tc.billingSchema, err)
			}
			if got != tc.want {
				t.Fatalf("resolveHostRiverSchema(%q, %q) = %q, want %q", tc.clientSchema, tc.billingSchema, got, tc.want)
			}
		})
	}
}
