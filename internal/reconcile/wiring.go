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

// BuildFetchersWithOptions is the strict builder used by operator commands.
// It respects rail role selection and returns config errors instead of
// silently picking an arbitrary account.
func BuildFetchersWithOptions(cfg *config.Config, rails config.RailSet, clients FetcherClients, d *db.DB, opts FetcherOptions) (map[Provider]RailFetcher, error) {
	fetchers := map[Provider]RailFetcher{}

	if key, c, err := selectNMIClient(rails, clients.NMIClients, opts.providerKey(ProviderNMI)); err != nil {
		return nil, err
	} else if c != nil {
		fetchers[ProviderNMI] = keyedFetcher{RailFetcher: NewNMIFetcher(c), key: key}
	}
	if clients.CCBillDataLink != nil {
		key, proc, err := selectRailByType(rails, config.RailTypeCCBill, opts.providerKey(ProviderCCBill))
		if err != nil {
			return nil, err
		}
		if proc != nil {
			fetchers[ProviderCCBill] = keyedFetcher{RailFetcher: NewCCBillFetcher(clients.CCBillDataLink), key: key}
		}
	}
	{
		key, sp, err := selectRailByType(rails, config.RailTypeStripe, opts.providerKey(ProviderStripe))
		if err != nil {
			return nil, err
		}
		if sp != nil && sp.SecretKey != "" {
			fetchers[ProviderStripe] = keyedFetcher{RailFetcher: NewStripeFetcher(sp.SecretKey), key: key}
		}
	}
	if clients.SolanaRPC != nil && d != nil {
		key, proc, err := selectRailByType(rails, config.RailTypeSolana, opts.providerKey(ProviderSolana))
		if err != nil {
			return nil, err
		}
		if proc != nil {
			fetchers[ProviderSolana] = keyedFetcher{RailFetcher: NewSolanaFetcher(clients.SolanaRPC, SolanaSubscriptionSourceFromDB(d)), key: key}
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

func selectRailByType(rails config.RailSet, providerType, explicitKey string) (string, *config.RailConfig, error) {
	if explicitKey != "" {
		proc := rails.GetRail(explicitKey)
		if proc == nil {
			return "", nil, fmt.Errorf("rail %q is not configured", explicitKey)
		}
		if proc.GetEffectiveType(explicitKey) != providerType {
			return "", nil, fmt.Errorf("rail %q is type %q, not %q", explicitKey, proc.GetEffectiveType(explicitKey), providerType)
		}
		return explicitKey, proc, nil
	}
	return rails.PrimaryRailByType(providerType)
}

func selectNMIClient(rails config.RailSet, clients map[string]*nmi.NMIClient, explicitKey string) (string, *nmi.NMIClient, error) {
	if explicitKey != "" {
		c := clients[explicitKey]
		if c == nil {
			return "", nil, fmt.Errorf("nmi rail %q is not configured for reconciliation", explicitKey)
		}
		return explicitKey, c, nil
	}
	key, proc, err := rails.PrimaryRailByType(config.RailTypeNMI)
	if err != nil {
		return "", nil, err
	}
	if proc != nil {
		c := clients[key]
		if c == nil {
			return "", nil, fmt.Errorf("primary nmi rail %q is not configured for reconciliation", key)
		}
		return key, c, nil
	}
	key, c := pickNMIClient(clients)
	return key, c, nil
}

// pickNMIClient selects the NMI gateway client deterministically: the only
// one when a single NMI rail is configured, else "mobius" then "nmi"
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
func NewEngine(d *db.DB, cfg *config.Config, fetchers map[Provider]RailFetcher) *Engine {
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
