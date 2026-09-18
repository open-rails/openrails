package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// UUIDs whose digit groups form a Luhn-valid run once the dashes are read as
// card formatting. Before the detector learned card grouping, each of these
// needed a per-field exemption to survive the firewall.
var luhnTrippingHandles = []string{
	"abcdefab-cdef-4abc-8111-111111111112",
	"a4111111-1111-4111-8119-abcdefabcdef",
	"a544fda7-1958-4199-9417-3263a6c4b369",
	"463d4942-14ef-4eec-9436-151088257692",
	"40ac552b-c48b-45fc-8968-269009312aae",
}

// Every identifier field now keeps the card scan: no field is exempt, and none
// needs to be.
func TestPANFirewallPassesStructuredHandles(t *testing.T) {
	t.Parallel()
	for _, handle := range append(luhnTrippingHandles, uuid.NewString()) {
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: handle}), handle)
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: "pm_" + handle}), handle)
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: handle}), handle)
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{PaymentToken: handle}), handle)
		require.NoError(t, RejectPANShapedFields(&CheckoutRequest{Metadata: map[string]string{"order_ref": handle}}), handle)
	}
}

// A card number is refused in every field, nested field and metadata entry,
// in every shape a card is written in.
func TestPANFirewallRefusesCardNumbersEverywhere(t *testing.T) {
	t.Parallel()
	const (
		visa       = "4111111111111111"
		mastercard = "5555 5555 5555 4444"
		amex       = "3782-822463-10005"
	)
	for _, card := range []string{visa, mastercard, amex} {
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{BTTokenIntentID: card}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{PaymentMethodID: "pm_" + card}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{PaymentToken: card}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{NameOnCard: "Cardholder " + card}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{Address1: "PO Box " + card}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{
			BTTokenIntentID: luhnTrippingHandles[0],
			Metadata:        map[string]string{"note": card},
		}), card)
		require.Error(t, RejectPANShapedFields(&CheckoutRequest{
			BTTokenIntentID: luhnTrippingHandles[0],
			Metadata:        map[string]string{card: "note"},
		}), card)
	}
}
