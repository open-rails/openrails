package reconcile

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// Fetcher/prober construction is per merchant (#699/#788): see
// MerchantFetcherBuilder in merchant_wiring.go — the armed rail state
// (psps + secret store) is the ONLY credential plane.

// NewEngine assembles a DB-backed engine over the given fetchers. The caller
// supplies a merchant-scoped context at Run time.
func NewEngine(d *db.DB, cfg *config.Config, fetchers map[Provider]RailFetcher) *Engine {
	e := &Engine{
		Fetchers: fetchers,
		Store:    &PGStore{DB: d},
		Local:    &PGLocalStateLoader{DB: d},
		Writer:   &PGLocalWriter{DB: d},
		// #665: subscription transitions route through the decider. No
		// deferred-delete scheduler here (CLI pulls log the wiring gap);
		// the river provider-refresh worker injects its own.
		Decisions: NewDecisionApplier(d, nil),
	}
	// Third dunning-forensics evidence source (#735): imported legacy history
	// + failed payments, read from Postgres.
	e.History = NewPGHistorySource(d)
	return e
}
