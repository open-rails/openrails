package reconcile

import (
	"fmt"
	"sort"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// Fetcher/prober construction is per merchant (#699): see
// MerchantFetcherBuilder in merchant_wiring.go. The selectors below resolve
// the BOOT-CONFIG fallback plane it uses when a merchant declares no account
// row on a rail.

func selectRailByType(rails config.RailMerchantAccountSet, rail models.Rail, explicitKey string) (string, *config.RailMerchantAccountConfig, error) {
	if explicitKey != "" {
		proc := rails.GetRail(explicitKey)
		if proc == nil {
			return "", nil, fmt.Errorf("provider account %q is not configured", explicitKey)
		}
		if proc.EffectiveRail(explicitKey) != rail {
			return "", nil, fmt.Errorf("provider account %q is on rail %q, not %q", explicitKey, proc.EffectiveRail(explicitKey), rail)
		}
		return explicitKey, proc, nil
	}
	return rails.ActiveRailByType(rail)
}

func selectNMIClient(rails config.RailMerchantAccountSet, clients map[string]*nmi.NMIClient, explicitKey string) (string, *nmi.NMIClient, error) {
	if explicitKey != "" {
		c := clients[explicitKey]
		if c == nil {
			return "", nil, fmt.Errorf("nmi rail %q is not configured for reconciliation", explicitKey)
		}
		return explicitKey, c, nil
	}
	key, proc, err := rails.ActiveRailByType(models.RailNMI)
	if err != nil {
		return "", nil, err
	}
	if proc != nil {
		// Runtime client maps are keyed by account_id (#641) with the active
		// account aliased under "nmi"; older sets keyed by the rail-set name.
		// Try all three before declaring the active rail unconfigured.
		for _, k := range []string{key, proc.EffectiveAccountID(), string(models.RailNMI)} {
			if k == "" {
				continue
			}
			if c := clients[k]; c != nil {
				return k, c, nil
			}
		}
		return "", nil, fmt.Errorf("active nmi rail %q is not configured for reconciliation", key)
	}
	key, c := pickNMIClient(clients)
	return key, c, nil
}

// pickNMIClient selects the NMI gateway client deterministically: the client
// keyed by the NMI rail, else the lexicographically first key.
func pickNMIClient(clients map[string]*nmi.NMIClient) (string, *nmi.NMIClient) {
	if len(clients) == 0 {
		return "", nil
	}
	if c, ok := clients[string(models.RailNMI)]; ok && c != nil {
		return string(models.RailNMI), c
	}
	keys := make([]string, 0, len(clients))
	for k := range clients {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0], clients[keys[0]]
}

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
