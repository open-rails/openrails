package reconcile

import (
	"fmt"
	"sort"
	"strings"

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

type FetcherOptions struct {
	// ProviderKeys optionally pins a provider type to a local config key. These
	// keys are selectors only; durable identity still comes from provider_accounts.
	ProviderKeys map[Provider]string
}

// BuildFetchers wires a ProcessorFetcher for every provider the runtime has
// clients/credentials for. Reads only; providers without configuration are
// simply absent from the map.
func BuildFetchers(cfg *config.Config, processors config.ProcessorSet, clients FetcherClients, d *db.DB) map[Provider]ProcessorFetcher {
	fetchers, _ := BuildFetchersWithOptions(cfg, processors, clients, d, FetcherOptions{})
	return fetchers
}

// BuildFetchersWithOptions is the strict builder used by operator commands.
// It respects processor role selection and returns config errors instead of
// silently picking an arbitrary account.
func BuildFetchersWithOptions(cfg *config.Config, processors config.ProcessorSet, clients FetcherClients, d *db.DB, opts FetcherOptions) (map[Provider]ProcessorFetcher, error) {
	fetchers := map[Provider]ProcessorFetcher{}

	if key, c, err := selectNMIClient(processors, clients.NMIClients, opts.providerKey(ProviderNMI)); err != nil {
		return nil, err
	} else if c != nil {
		fetchers[ProviderNMI] = keyedFetcher{ProcessorFetcher: NewNMIFetcher(c), key: key}
	}
	if clients.CCBillDataLink != nil {
		key, proc, err := selectProcessorByType(processors, config.ProcessorTypeCCBill, opts.providerKey(ProviderCCBill))
		if err != nil {
			return nil, err
		}
		if proc != nil {
			fetchers[ProviderCCBill] = keyedFetcher{ProcessorFetcher: NewCCBillFetcher(clients.CCBillDataLink), key: key}
		}
	}
	{
		key, sp, err := selectProcessorByType(processors, config.ProcessorTypeStripe, opts.providerKey(ProviderStripe))
		if err != nil {
			return nil, err
		}
		if sp != nil && sp.SecretKey != "" {
			fetchers[ProviderStripe] = keyedFetcher{ProcessorFetcher: NewStripeFetcher(sp.SecretKey), key: key}
		}
	}
	if clients.SolanaRPC != nil && d != nil {
		key, proc, err := selectProcessorByType(processors, config.ProcessorTypeSolana, opts.providerKey(ProviderSolana))
		if err != nil {
			return nil, err
		}
		if proc != nil {
			fetchers[ProviderSolana] = keyedFetcher{ProcessorFetcher: NewSolanaFetcher(clients.SolanaRPC, SolanaSubscriptionSourceFromDB(d)), key: key}
		}
	}

	return fetchers, nil
}

func (o FetcherOptions) providerKey(provider Provider) string {
	if o.ProviderKeys == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(o.ProviderKeys[provider]))
}

func selectProcessorByType(processors config.ProcessorSet, providerType, explicitKey string) (string, *config.ProcessorConfig, error) {
	if explicitKey != "" {
		proc := processors.GetProcessor(explicitKey)
		if proc == nil {
			return "", nil, fmt.Errorf("processor %q is not configured", explicitKey)
		}
		if proc.GetEffectiveType(explicitKey) != providerType {
			return "", nil, fmt.Errorf("processor %q is type %q, not %q", explicitKey, proc.GetEffectiveType(explicitKey), providerType)
		}
		return explicitKey, proc, nil
	}
	return processors.PrimaryProcessorByType(providerType)
}

func selectNMIClient(processors config.ProcessorSet, clients map[string]*nmi.NMIClient, explicitKey string) (string, *nmi.NMIClient, error) {
	if explicitKey != "" {
		c := clients[explicitKey]
		if c == nil {
			return "", nil, fmt.Errorf("nmi processor %q is not configured for reconciliation", explicitKey)
		}
		return explicitKey, c, nil
	}
	key, proc, err := processors.PrimaryProcessorByType(config.ProcessorTypeNMI)
	if err != nil {
		return "", nil, err
	}
	if proc != nil {
		c := clients[key]
		if c == nil {
			return "", nil, fmt.Errorf("primary nmi processor %q is not configured for reconciliation", key)
		}
		return key, c, nil
	}
	key, c := pickNMIClient(clients)
	return key, c, nil
}

// pickNMIClient selects the NMI gateway client deterministically: the only
// one when a single NMI processor is configured, else "mobius" then "nmi"
// then the lexicographically first key.
func pickNMIClient(clients map[string]*nmi.NMIClient) (string, *nmi.NMIClient) {
	if len(clients) == 0 {
		return "", nil
	}
	for _, preferred := range []string{"mobius", "nmi"} {
		if c, ok := clients[preferred]; ok && c != nil {
			return preferred, c
		}
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
func NewEngine(d *db.DB, cfg *config.Config, fetchers map[Provider]ProcessorFetcher) *Engine {
	e := &Engine{
		Fetchers: fetchers,
		Store:    &PGStore{DB: d},
		Local:    &PGLocalStateLoader{DB: d},
		Writer:   &PGLocalWriter{DB: d},
		Intents:  &PGStuckIntentSource{DB: d},
	}
	if cfg != nil {
		// Third dunning-forensics evidence source: OpenRails' own ClickHouse
		// analytics events (incl. imported legacy history). Optional — when
		// ClickHouse is absent the report carries a note instead.
		if cfg.ClickHouse != nil {
			e.History = NewAnalyticsHistorySource(analytics.NewDunningHistoryService(cfg.ClickHouse))
		}
	}
	return e
}
