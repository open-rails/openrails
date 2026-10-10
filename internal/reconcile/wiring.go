package reconcile

import (
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Fetchers and probers are built per merchant (MerchantFetcherBuilder): armed
// PSPs and the secret store are the only credential source.

// NewEngine assembles a DB-backed engine over the given fetchers. cancels
// queues the provider cancel of a terminal decision; every pull (worker or
// CLI) supplies the system-origin scheduler. The caller supplies a
// merchant-scoped context at Run time.
func NewEngine(d *db.DB, cfg *config.Config, contacts identity.Directory, fetchers map[Provider]RailFetcher, cancels subscriptions.ProviderCancelScheduler) *Engine {
	decisions := NewDecisionApplier(d, cancels)
	decisions.LC.SetConfig(cfg)
	e := &Engine{
		Fetchers: fetchers,
		Store:    &PGStore{DB: d},
		Local:    &PGLocalStateLoader{DB: d, Contacts: contacts},
		Writer:   &PGLocalWriter{DB: d},
		// Subscription transitions route through the decider.
		Decisions: decisions,
		// Evidence-staleness floor, read per run from the destructive policy.
		Policy: destructive.New(d),
		// Every enforce pass that overwrites subscription state opens a
		// destructive run with before-images, so `openrails undo-run --run <id>`
		// can put the book back.
		Runs: &PGDestructiveRunRecorder{DB: d},
	}
	// Third dunning-forensics evidence source: failed payment attempts.
	e.History = NewPGHistorySource(d)
	return e
}
