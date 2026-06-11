package reconcile

import (
	"sort"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/analytics"
)

// FetcherClients are the already-built runtime clients the fetchers wrap.
// Every dependency is read-only from the engine's perspective: the NMI
// fetcher only reaches query.php, CCBill only DataLink exports, Stripe only
// GETs through the write-blocked stripeapi transport, Solana only RPC reads.
type FetcherClients struct {
	NMIClients     map[string]*nmi.NMIClient
	CCBillDataLink *ccbill.DataLinkClient
	SolanaRPC      *solanaint.RPCClient
}

// BuildFetchers wires a ProcessorFetcher for every provider the runtime has
// clients/credentials for. Reads only; providers without configuration are
// simply absent from the map.
func BuildFetchers(cfg *config.Config, clients FetcherClients, d *db.DB) map[Provider]ProcessorFetcher {
	fetchers := map[Provider]ProcessorFetcher{}

	if c := pickNMIClient(clients.NMIClients); c != nil {
		fetchers[ProviderNMI] = NewNMIFetcher(c)
	}
	if clients.CCBillDataLink != nil {
		fetchers[ProviderCCBill] = NewCCBillFetcher(clients.CCBillDataLink)
	}
	if cfg != nil {
		if sp := cfg.GetStripeProcessor(); sp != nil && sp.SecretKey != "" {
			fetchers[ProviderStripe] = NewStripeFetcher(sp.SecretKey)
		}
	}
	if clients.SolanaRPC != nil && d != nil {
		fetchers[ProviderSolana] = NewSolanaFetcher(clients.SolanaRPC, SolanaSubscriptionSourceFromDB(d))
	}

	return fetchers
}

// pickNMIClient selects the NMI gateway client deterministically: the only
// one when a single NMI processor is configured, else "mobius" then "nmi"
// then the lexicographically first key.
func pickNMIClient(clients map[string]*nmi.NMIClient) *nmi.NMIClient {
	if len(clients) == 0 {
		return nil
	}
	for _, preferred := range []string{"mobius", "nmi"} {
		if c, ok := clients[preferred]; ok && c != nil {
			return c
		}
	}
	keys := make([]string, 0, len(clients))
	for k := range clients {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return clients[keys[0]]
}

// NewEngine assembles a DB-backed engine over the given fetchers. The caller
// supplies a tenant-scoped context at Run time.
func NewEngine(d *db.DB, cfg *config.Config, fetchers map[Provider]ProcessorFetcher) *Engine {
	e := &Engine{
		Fetchers: fetchers,
		Store:    &PGStore{DB: d},
		Local:    &PGLocalStateLoader{DB: d},
		Writer:   &PGLocalWriter{DB: d},
	}
	if cfg != nil {
		e.DisableEntitlementRevocation = cfg.IsEntitlementExpirationDisabled()
		// Third dunning-forensics evidence source: OpenRails' own ClickHouse
		// analytics events (incl. imported legacy history). Optional — when
		// ClickHouse is absent the report carries a note instead.
		if cfg.ClickHouse != nil {
			e.History = NewAnalyticsHistorySource(analytics.NewDunningHistoryService(cfg.ClickHouse))
		}
	}
	return e
}
