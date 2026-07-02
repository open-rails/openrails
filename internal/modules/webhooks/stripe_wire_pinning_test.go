package webhooks

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// #671 TEST WALL — inbound Stripe wire pinning: a known cents value in the
// webhook JSON must become the EXACT micros value stored on the payment row.
// The JSON is parsed with the real stripeInvoice type so both the wire field
// name AND the conversion factor are pinned.

func parseStripeInvoiceJSON(t *testing.T, raw string) stripeInvoice {
	t.Helper()
	var inv stripeInvoice
	require.NoError(t, json.Unmarshal([]byte(raw), &inv))
	return inv
}

func TestStripeInvoicePaidAmountMicros_WirePin(t *testing.T) {
	cases := []struct {
		name string
		json string
		want int64 // micros, literal — never derived via the converter under test
	}{
		{
			name: "19.99 charge: 1999 cents on the wire, 19_990_000 micros stored",
			json: `{"id":"in_1","amount_paid":1999,"currency":"usd"}`,
			want: 19_990_000,
		},
		{
			name: "one cent",
			json: `{"id":"in_1","amount_paid":1}`,
			want: 10_000,
		},
		{
			name: "zero (trial invoice)",
			json: `{"id":"in_1","amount_paid":0}`,
			want: 0,
		},
		{
			name: "amount_due must NOT leak into the success amount",
			json: `{"id":"in_1","amount_paid":1999,"amount_due":2500}`,
			want: 19_990_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := parseStripeInvoiceJSON(t, tc.json)
			require.Equal(t, tc.want, stripeInvoicePaidAmountMicros(inv))
		})
	}
}

func TestStripeInvoiceFailedAmountMicros_WirePin(t *testing.T) {
	// #671 1f / #675: the failed-payment row records amount_due (what was
	// attempted) converted to micros — never the raw cents value.
	cases := []struct {
		name string
		json string
		want int64
	}{
		{
			name: "12.00 failed attempt: 1200 cents on the wire, 12_000_000 micros stored",
			json: `{"id":"in_1","subscription":"sub_1","amount_due":1200,"amount_paid":0,"currency":"usd"}`,
			want: 12_000_000,
		},
		{
			name: "19.99 failed attempt",
			json: `{"id":"in_1","amount_due":1999,"amount_paid":0}`,
			want: 19_990_000,
		},
		{
			name: "amount_paid must NOT leak into the failed amount",
			json: `{"id":"in_1","amount_due":1200,"amount_paid":9999}`,
			want: 12_000_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := parseStripeInvoiceJSON(t, tc.json)
			require.Equal(t, tc.want, stripeInvoiceFailedAmountMicros(inv))
		})
	}
}
