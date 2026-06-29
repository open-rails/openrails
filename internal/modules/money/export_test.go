//go:build integration

package money

import "time"

// MeteredPeriodSourceIDForTest exposes the deterministic period source_id used by
// the #615 usage->owed sweep so integration tests can assert idempotency without
// hardcoding the internal format.
func MeteredPeriodSourceIDForTest(from, to time.Time) string {
	return meteredPeriodSourceID(from, to)
}
