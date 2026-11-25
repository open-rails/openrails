package eventstore

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// usageTables are the usage family's tables: raw events, then the rollups.
var usageTables = []string{"usage_events", "limit_usage", "usage_quantities", "usage_minutes", "usage_history"}

// exec runs a statement that names the context's merchant, waiting for a
// mutation to finish.
func (s *Store) mutate(ctx context.Context, what, stmt string) error {
	return fail(what, s.conn.Exec(ctx, s.resolve(stmt)+" SETTINGS mutations_sync = 2"))
}

// EraseCustomer clears the invoker and metadata of a customer's usage events
// and folds its history's invokers into one: quantities, charges and what
// posted stay. The caller holds the customer's lock, so no intake adds to the
// history meanwhile. A run stopped part way is run again to the same end: the
// events' update is idempotent; the fold inserts, for each invoker's history
// rows that still sum to anything, their negation and their sum under no
// invoker, in one insert; then the rows naming an invoker, all folded, are
// deleted.
func (s *Store) EraseCustomer(ctx context.Context, customer billing.CustomerID) error {
	mid, err := s.merchant(ctx)
	if err != nil {
		return err
	}
	who := fmt.Sprintf("merchant_id = %s AND customer_id = %s", mid, uuidLiteral(uuid.UUID(customer)))
	if err := s.mutate(ctx, "erase usage events", "ALTER TABLE {database}.usage_events UPDATE invoker = '', metadata = '' WHERE "+who+" AND (invoker != '' OR metadata != '')"); err != nil {
		return err
	}
	var named bool
	if err := s.query(ctx, "read usage history", "SELECT count() > 0 FROM {database}.usage_history WHERE "+who+" AND invoker != ''",
		func(rows interface{ Scan(...any) error }) error { return rows.Scan(&named) }); err != nil || !named {
		return err
	}
	keys := "merchant_id, customer_id, day_at, bucket_at, currency, event_type, meter, cell, print, period_starts_at"
	if err := fail("erase usage history", s.conn.Exec(ctx, s.resolve(`INSERT INTO {database}.usage_history (`+keys+`, invoker, outcome, requests, quantity, stated, charged, forgiven)
SELECT `+keys+`, fold.1, outcome, fold.2 * r, fold.2 * q, fold.2 * st, fold.2 * c, fold.2 * f
FROM (SELECT `+keys+`, invoker, outcome, sum(requests) AS r, sum(quantity) AS q, sum(stated) AS st, sum(charged) AS c, sum(forgiven) AS f
      FROM {database}.usage_history WHERE `+who+` AND invoker != '' GROUP BY `+keys+`, invoker, outcome
      HAVING r != 0 OR q != 0 OR st != 0 OR c != 0 OR f != 0)
ARRAY JOIN [(invoker, toInt64(-1)), ('', toInt64(1))] AS fold
SETTINGS max_insert_threads = 1`))); err != nil {
		return err
	}
	return s.mutate(ctx, "erase usage history", "ALTER TABLE {database}.usage_history DELETE WHERE "+who+" AND invoker != ''")
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
