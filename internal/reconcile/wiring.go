package reconcile

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// Fetcher/prober construction is per merchant (#699/#788): see
// MerchantFetcherBuilder in merchant_wiring.go — the armed rail state
// (psps + secret store) is the ONLY credential plane.

// NewEngine assembles a DB-backed engine over the given fetchers. cancels
// queues the provider cancel of a terminal decision; every pull (worker or
// CLI) supplies the system-origin scheduler. The caller supplies a
// merchant-scoped context at Run time.
func NewEngine(d *db.DB, cfg *config.Config, fetchers map[Provider]RailFetcher, cancels subscriptions.ProviderCancelScheduler) *Engine {
	e := &Engine{
		Fetchers: fetchers,
		Store:    &PGStore{DB: d},
		Local:    &PGLocalStateLoader{DB: d},
		Writer:   &PGLocalWriter{DB: d},
		// #665: subscription transitions route through the decider.
		Decisions: NewDecisionApplier(d, cancels),
		// #835 evidence-staleness floor, read per run from the merchant's
		// destructive policy.
		Policy: destructive.New(d),
		// or#859: every enforce pass that overwrites subscription state opens a
		// destructive run and captures before-images, so `openrails converge
		// rollback --run <id>` can put the book back.
		Runs: &PGDestructiveRunRecorder{DB: d},
	}
	// Third dunning-forensics evidence source (#735): imported legacy history
	// + failed payments, read from Postgres.
	e.History = NewPGHistorySource(d)
	return e
}
