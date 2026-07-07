package app

import (
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// newIntentDeferredDeleteScheduler builds the intent-ledger-backed deferred
// NMI delete scheduler. The implementation moved to intents.NMIDeleteScheduler
// (#679) so the unknown-reconcile path and tests can construct the REAL
// production scheduler; this shim keeps the composition root unchanged.
//
// Every producer writes the DeletionScheduledAt marker and the nmi_delete
// intent in ONE transaction (runtime paths since #679/#138; the DeclaredBilling
// import since #138), so marker and intent cannot diverge — the old
// out-of-band startup marker sweep is gone.
func newIntentDeferredDeleteScheduler(d *db.DB, ceiling *intents.RateCeiling, origin intents.Origin, reason string) subscriptions.DeferredDeleteScheduler {
	return intents.NewNMIDeleteScheduler(d, ceiling, origin, reason)
}
