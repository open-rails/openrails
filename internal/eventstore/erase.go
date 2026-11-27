package eventstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// usageTables are the usage family's tables: raw events, then the rollups.
var usageTables = []string{"usage_events", "limit_usage", "usage_history"}

// exec runs a statement that names the context's merchant, waiting for a
// mutation to finish.
func (s *Store) mutate(ctx context.Context, what, stmt string) error {
	return fail(what, s.conn.Exec(ctx, s.resolve(stmt)+" SETTINGS mutations_sync = 2"))
}

// EraseCustomer clears the invoker and metadata of a customer's usage
// events; quantities, charges and what posted stay. History names no
// invoker, so it keeps the customer's usage as it is. The update is
// idempotent: a run stopped part way is run again to the same end.
func (s *Store) EraseCustomer(ctx context.Context, customer billing.CustomerID) error {
	mid, err := s.merchant(ctx)
	if err != nil {
		return err
	}
	who := fmt.Sprintf("merchant_id = %s AND customer_id = %s", mid, uuidLiteral(uuid.UUID(customer)))
	return s.mutate(ctx, "erase usage events", "ALTER TABLE {database}.usage_events UPDATE invoker = '', metadata = '' WHERE "+who+" AND (invoker != '' OR metadata != '')")
}

// DeleteMerchant deletes every usage row of the context's merchant.
func (s *Store) DeleteMerchant(ctx context.Context) error {
	mid, err := s.merchant(ctx)
	if err != nil {
		return err
	}
	for _, table := range usageTables {
		if err := s.mutate(ctx, "delete "+table, "ALTER TABLE {database}."+table+" DELETE WHERE merchant_id = "+mid); err != nil {
			return err
		}
	}
	return nil
}

// HasUsage reports whether the context's merchant has any usage in the store.
func (s *Store) HasUsage(ctx context.Context) (bool, error) {
	mid, err := s.merchant(ctx)
	if err != nil {
		return false, err
	}
	var found bool
	err = s.query(ctx, "read usage", "SELECT count() > 0 FROM (SELECT 1 FROM {database}.usage_events WHERE merchant_id = "+mid+
		" LIMIT 1 UNION ALL SELECT 1 FROM {database}.usage_history WHERE merchant_id = "+mid+" LIMIT 1)",
		func(rows interface{ Scan(...any) error }) error { return rows.Scan(&found) })
	return found, err
}
