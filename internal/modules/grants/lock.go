package grants

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// LockCustomer takes the per-customer spend lock (the customers row FOR UPDATE
// money's lockBalance takes), so grant repairs serialize with the customer's
// money movements. Only effective inside a tx.
func (l *Ledger) LockCustomer(ctx context.Context, customer uuid.UUID) error {
	if _, err := l.q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{
		ID: customer, MerchantID: l.merchant,
	}); err != nil {
		return fmt.Errorf("grants: lock customer %s: %w", customer, err)
	}
	return nil
}
