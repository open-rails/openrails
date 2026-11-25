package catalogrules

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestSourceRef(t *testing.T) {
	payment, line := uuid.New(), uuid.New()
	require.Equal(t, billing.PaymentID(payment).String(), SourceRef("purchase", payment.String(), &payment))
	require.Equal(t, billing.InvoiceLineID(line).String(), SourceRef("purchase", line.String(), &payment))
	require.Equal(t, billing.InvoiceLineID(line).String(), SourceRef("purchase", line.String(), nil))
	require.Equal(t, billing.SubscriptionID(line).String(), SourceRef("subscription", line.String(), &payment))
	require.Equal(t, "include:bundle", SourceRef("purchase", "include:bundle", &payment))
}
