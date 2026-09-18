package contract

import (
	"bytes"
	"io"
	"testing"

	"github.com/open-rails/openrails/internal/archivewire"
)

func TestWireIntegrityDoesNotAuthorizeBillingContent(t *testing.T) {
	var tables []string
	for _, p := range Profiles {
		tables = append(tables, p.Name)
	}
	mid, customer := testMerchant, "10000000-0000-0000-0000-000000000002"
	issuer, timestamp := "https://merchant.example", "2026-09-17 12:00:00+00"
	unsafe := "4111111111111111"
	for _, tc := range []struct {
		name   string
		tables []string
		row    []*string
		valid  bool
	}{
		{"supported", tables, []*string{&mid, &customer, &issuer, &timestamp, &timestamp}, true},
		{"unknown table", []string{"merchant_secrets"}, nil, false},
		{"missing tables", tables[:len(tables)-1], nil, false},
		{"wrong order", append([]string{tables[1], tables[0]}, tables[2:]...), nil, false},
		{"extra table", append(append([]string{}, tables...), tables[0]), nil, false},
		{"wrong row width", tables, []*string{&mid}, false},
		{"unsafe value", tables, []*string{&mid, &customer, &unsafe, &timestamp, &timestamp}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var artifact bytes.Buffer
			w, err := archivewire.NewWriter(&artifact, mid)
			if err != nil {
				t.Fatal(err)
			}
			for i, table := range tc.tables {
				if err := w.Table(table); err != nil {
					t.Fatal(err)
				}
				if i == 0 && tc.row != nil {
					if err := w.Row(tc.row); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := archivewire.CopyVerified(io.Discard, bytes.NewReader(artifact.Bytes())); err != nil {
				t.Fatalf("complete artifact failed wire integrity: %v", err)
			}
			if _, err := Read(bytes.NewReader(artifact.Bytes()), nil, nil); (err == nil) != tc.valid {
				t.Fatalf("billing contract accepted=%t, want %t: %v", err == nil, tc.valid, err)
			}
		})
	}
}
