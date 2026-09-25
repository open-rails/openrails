package app

import (
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// newProviderCancelScheduler builds the intent-ledger-backed provider-cancel
// scheduler (intents.ProviderCancelScheduler).
func newProviderCancelScheduler(d *db.DB, ceiling *intents.RateCeiling, origin intents.Origin, reason string) subscriptions.ProviderCancelScheduler {
	return intents.NewProviderCancelScheduler(d, ceiling, origin, reason)
}
