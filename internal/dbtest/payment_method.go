package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// SeedNMIStoredCredentialRefs makes an integration-test payment method model
// completed customer-present transactions for both agreement types. Production
// code must capture these refs from NMI; tests that exercise subsequent
// CIT/MIT charges need explicit, non-empty anchors instead of the historical
// reference-less fixture shape.
func SeedNMIStoredCredentialRefs(ctx context.Context, t testing.TB, qx gen.DBTX, paymentMethodID uuid.UUID) {
	t.Helper()
	_, err := qx.Exec(ctx, `
		UPDATE openrails.payment_methods
		SET stored_credential_recurring_ref = $2,
		    stored_credential_unscheduled_ref = $3
		WHERE id = $1`,
		paymentMethodID,
		"txn_recurring_initial_"+paymentMethodID.String(),
		"txn_unscheduled_initial_"+paymentMethodID.String())
	require.NoError(t, err)
}
