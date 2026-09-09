//go:build integration

package money

import (
	"time"

	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// NewNMICollectionAdapters builds the per-rail adapter map used by integration
// fixtures. Production wiring constructs its adapters through the merchant
// collection registry instead.
func NewNMICollectionAdapters(clients map[string]*nmi.NMIClient) map[string]CollectionAdapter {
	adapters := make(map[string]CollectionAdapter, len(clients))
	for rail, client := range clients {
		rail = normalizeRail(rail)
		if rail == "" || client == nil {
			continue
		}
		adapters[rail] = NewNMICollectionAdapter(client)
	}
	return adapters
}

// MeteredPeriodSourceIDForTest exposes the deterministic period source_id used by
// the #615 usage->owed sweep so integration tests can assert idempotency without
// hardcoding the internal format.
func MeteredPeriodSourceIDForTest(from, to time.Time) string {
	return meteredPeriodSourceID(from, to)
}
