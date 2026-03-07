package webhookpayload

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalProvider(t *testing.T) {
	require.Equal(t, "mobius", CanonicalProvider("nmi"))
	require.Equal(t, "mobius", CanonicalProvider(" Mobius "))
	require.Equal(t, "stripe", CanonicalProvider("/stripe/"))
}

func TestParseStripeEventMeta(t *testing.T) {
	eventID, eventType, err := ParseStripeEventMeta([]byte(`{"id":"evt_123","type":"invoice.paid"}`))
	require.NoError(t, err)
	require.Equal(t, "evt_123", eventID)
	require.Equal(t, "invoice.paid", eventType)
}

func TestComputeUniqueKeyUsesEventIDWhenPresent(t *testing.T) {
	require.Equal(t, "webhook:mobius:evt_123", ComputeUniqueKey("nmi", "evt_123", "", []byte(`{}`)))
	require.NotEmpty(t, ComputeUniqueKey("ccbill", "", "NewSaleSuccess", []byte(`{"a":1}`)))
}

func TestNormalizeCCBillPayload(t *testing.T) {
	normalized, err := NormalizeCCBillPayload([]byte("eventType=NewSaleSuccess&subscriptionId=123"))
	require.NoError(t, err)
	require.JSONEq(t, `{"eventType":"NewSaleSuccess","subscriptionId":"123"}`, string(normalized))

	normalized, err = NormalizeCCBillPayload([]byte(`{"ok":true}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(normalized))
}
