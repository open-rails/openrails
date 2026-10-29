package reconcile

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
)

// The pull plane (provider refresh, unknown-cohort resolution, per-sub probes,
// operator pulls) arms per merchant from psps rows and the merchant secret
// store; a rail with no declared account is not armed. Missing secrets fail
// loud: the rail's fetcher/prober is absent and one WARN names the merchant,
// rail and secret, never a default-allow. Secrets resolve at use time inside
// the merchant scope; nothing is cached process-wide.

// MerchantPullClients is one merchant's armed pull plane.
type MerchantPullClients struct {
	Fetchers map[Provider]RailFetcher
	Probers  map[Provider]SubscriptionProber
	// Coverage records, per armed rail, how many PSPs the merchant declares
	// active versus how many this pass read. A pull arms from one PSP, so with
	// two NMI PSPs an "exhaustive" roster would cancel every subscription of
	// the unread one.
	Coverage map[Provider]PSPCoverage
	// Unavailable marks rails that did not arm because the merchant's
	// configuration could not be read: a retry may arm them. A rail absent
	// from Fetchers and from Unavailable is one the merchant has not armed.
	Unavailable map[Provider]bool
}

// PSPCoverage is one rail's PSP coverage for a pull pass.
type PSPCoverage struct {
	// Declared is how many PSPs are active for new work on the rail.
	Declared int
	// Pulled is how many of them this pass read (a fetcher arms from one).
	Pulled int
	// Binding is the PSP the pass armed from.
	Binding PSPBinding
}

// Complete reports whether the pass read every active PSP on the rail — the
// precondition for treating its roster as an absence proof.
func (c PSPCoverage) Complete() bool { return c.Declared > 0 && c.Pulled >= c.Declared }

// ProviderEndpoints overrides provider base URLs on store-armed clients — a
// test seam for fake provider HTTP servers. Zero value = real endpoints.
type ProviderEndpoints struct {
	NMIQueryURL           string
	NMIV5BaseURL          string
	CCBillDataLinkBaseURL string
	StripeBaseURL         string
}

// MerchantFetcherBuilder builds one merchant's fetchers/probers at use time
// from the armed rail state (nil Merchants = nothing arms; fail closed).
type MerchantFetcherBuilder struct {
	StripeClients *stripeapi.Factory
	Config        *config.Config
	Merchants     *merchants.Service
	DB            *db.DB

	// AccountIDs optionally pins a rail to one declared account_id (operator
	// pulls; may target archived accounts for drain).
	AccountIDs map[Provider]string

	Endpoints ProviderEndpoints
	// NMIClients is the runtime's shared NMI client factory; its transport
	// (the fake-provider seam) carries over to pull clients.
	NMIClients *railresolve.NMIFactory
}

// Build resolves the merchant's pull credentials and returns the armed plane.
// It never fails as a whole: a rail that cannot arm is absent (with its WARN
// already logged) so the remaining rails keep pulling.
func (b MerchantFetcherBuilder) Build(ctx context.Context, mid billing.MerchantID) MerchantPullClients {
	out := newPullClients()
	for _, provider := range []Provider{ProviderNMI, ProviderCCBill, ProviderStripe, ProviderSolana} {
		if provider == ProviderSolana && b.DB == nil {
			continue
		}
		if scope, ok := b.resolveScopeCoverage(ctx, mid, provider, &out); ok {
			b.arm(ctx, mid, scope, &out)
		}
	}
	return out
}

// BuildPSP arms the one PSP that owns an object, archived or not: a read about
// a subscription binds to its own account, never to a sibling on the rail.
// ok=false when the PSP cannot arm (its WARN is logged): nothing is read, so
// nothing is decided.
func (b MerchantFetcherBuilder) BuildPSP(ctx context.Context, mid billing.MerchantID, pspID uuid.UUID) (MerchantPullClients, Provider, bool) {
	out := newPullClients()
	if b.Merchants == nil {
		return out, "", false
	}
	scope, ok, err := b.Merchants.PSPScopeByID(ctx, mid, pspID)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("PSP is not declared for this merchant")
		}
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{"merchant_id": mid.String(), "psp_id": pspID.String()}).
			Warn("provider pull: PSP not armed — its objects are not read")
		return out, "", false
	}
	provider := Provider(scope.Rail)
	out.Coverage[provider] = PSPCoverage{Declared: 1, Pulled: 1, Binding: PSPBinding{ID: scope.ID, Rail: scope.Rail, AccountID: scope.AccountID}}
	b.arm(ctx, mid, scope, &out)
	_, fetches := out.Fetchers[provider]
	_, probes := out.Probers[provider]
	return out, provider, fetches || probes
}

func newPullClients() MerchantPullClients {
	return MerchantPullClients{
		Fetchers:    map[Provider]RailFetcher{},
		Probers:     map[Provider]SubscriptionProber{},
		Coverage:    map[Provider]PSPCoverage{},
		Unavailable: map[Provider]bool{},
	}
}

func (b MerchantFetcherBuilder) arm(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, out *MerchantPullClients) {
	switch Provider(scope.Rail) {
	case ProviderNMI:
		b.buildNMI(ctx, mid, scope, out)
	case ProviderCCBill:
		b.buildCCBill(ctx, mid, scope, out)
	case ProviderStripe:
		b.buildStripe(ctx, mid, scope, out)
	case ProviderSolana:
		b.buildSolana(ctx, mid, scope, out)
	}
}

func (b MerchantFetcherBuilder) readOnly() bool {
	return b.Config != nil && config.IsProviderReadOnly(b.Config)
}

// resolveScopeCoverage is resolveScope plus the coverage record: how many PSPs
// the merchant declares active on the rail versus the one this pass arms from.
func (b MerchantFetcherBuilder) resolveScopeCoverage(ctx context.Context, mid billing.MerchantID, provider Provider, out *MerchantPullClients) (merchants.PSPScope, bool) {
	scope, ok, err := b.resolveScopeInner(ctx, mid, provider)
	if err != nil {
		b.warnScopeError(ctx, mid, provider, err)
		out.markUnavailable(provider, err)
		return merchants.PSPScope{}, false
	}
	if !ok {
		return scope, false
	}
	declared := 1
	if b.Merchants != nil {
		if n, err := b.Merchants.CountActivePSPsForRail(ctx, mid, string(provider)); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": string(provider),
			}).Warn("provider pull: active-PSP count failed; treating the rail as partially covered (absence proves nothing)")
			declared = 2 // fail safe: unknown coverage is INCOMPLETE coverage
		} else if n > 0 {
			declared = n
		}
	}
	if declared > 1 {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": string(provider), "active_psps": declared,
		}).Warn("provider pull: rail has multiple active PSPs but a pull arms from ONE — its roster cannot prove absence for the siblings it did not read")
	}
	out.Coverage[provider] = PSPCoverage{
		Declared: declared,
		Pulled:   1,
		Binding:  PSPBinding{ID: scope.ID, Rail: scope.Rail, AccountID: scope.AccountID},
	}
	return scope, true
}

func (b MerchantFetcherBuilder) resolveScopeInner(ctx context.Context, mid billing.MerchantID, provider Provider) (merchants.PSPScope, bool, error) {
	if b.Merchants == nil {
		return merchants.PSPScope{}, false, nil
	}
	rail := string(provider)
	if pin := strings.TrimSpace(b.AccountIDs[provider]); pin != "" {
		scope, ok, err := b.Merchants.PSPScopeByAccountID(ctx, mid, rail, pin)
		if err == nil && !ok {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": rail, "account_id": pin,
			}).Warn("provider pull: pinned PSP is not declared for this merchant")
		}
		return scope, ok, err
	}
	return b.Merchants.PullPSPScope(ctx, mid, rail)
}

// markUnavailable records a rail left unarmed because the merchant's
// configuration could not be read, never because it is not credentialed.
func (out *MerchantPullClients) markUnavailable(provider Provider, err error) {
	if errors.Is(err, merchants.ErrConfigUnavailable) {
		out.Unavailable[provider] = true
	}
}

func (b MerchantFetcherBuilder) warnScopeError(ctx context.Context, mid billing.MerchantID, provider Provider, err error) {
	log.WithContext(ctx).WithError(err).WithFields(log.Fields{
		"merchant_id": mid.String(), "rail": string(provider),
	}).Warn("provider pull: rail not armed — PSP resolution failed")
}

// secret loads one scoped secret. found=false with nil err means the secret is
// genuinely absent (terminal for this pass); backend errors surface as err.
func (b MerchantFetcherBuilder) secret(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, key string) (string, bool, error) {
	if b.Merchants == nil || b.Merchants.Secrets() == nil {
		return "", false, nil
	}
	// Honour the PSP row's rotation version floor.
	ref, err := scope.SecretRef(key)
	if err != nil {
		return "", false, err
	}
	sec, err := merchants.ReadSecretRef(ctx, b.Merchants.Secrets(), mid, ref)
	if errors.Is(err, merchants.ErrSecretNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", false, nil
	}
	return value, true, nil
}

// requireSecret is secret, failing loud: absence or a backend failure logs one
// WARN naming merchant, rail and the secret name.
func (b MerchantFetcherBuilder) requireSecret(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, key string, out *MerchantPullClients) (string, bool) {
	value, found, err := b.secret(ctx, mid, scope, key)
	if err != nil {
		out.markUnavailable(Provider(scope.Rail), err)
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant configuration unavailable (retried next pass)")
		return "", false
	}
	if !found {
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant secret missing")
		return "", false
	}
	return value, true
}

func (b MerchantFetcherBuilder) buildNMI(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, out *MerchantPullClients) {
	if _, ok := b.requireSecret(ctx, mid, scope, "security_key", out); !ok {
		return // fail closed (logged): a declared account never falls back across planes
	}
	factory := &railresolve.NMIFactory{Config: b.Config, Endpoints: railresolve.NMIEndpoints{QueryURL: b.Endpoints.NMIQueryURL, V5BaseURL: b.Endpoints.NMIV5BaseURL}}
	if b.NMIClients != nil {
		factory.Transport = b.NMIClients.Transport
	}
	client, err := factory.Client(ctx, b.Merchants.Secrets(), mid, scope)
	if err != nil {
		out.markUnavailable(ProviderNMI, err)
		// Fail closed: a declared account never falls back across planes.
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": "nmi",
		}).Warn("provider pull: rail not armed — NMI client build failed")
		return
	}
	out.Fetchers[ProviderNMI] = keyedFetcher{RailFetcher: NewNMIFetcher(client), key: scope.AccountID}
	out.Probers[ProviderNMI] = &NMISubscriptionProber{Client: client}
}

func (b MerchantFetcherBuilder) buildCCBill(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, out *MerchantPullClients) {
	// CCBill account_id is dash-joined (clientAccnum-clientSubacc, e.g.
	// 999999-0000). Both parts are numeric, so the first dash splits.
	acc, sub, cut := strings.Cut(strings.TrimSpace(scope.AccountID), "-")
	if !cut || strings.TrimSpace(acc) == "" || strings.TrimSpace(sub) == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": "ccbill", "account_id": scope.AccountID,
		}).Warn("provider pull: rail not armed — CCBill account_id must be clientAccnum-clientSubacc (e.g. 999999-0000)")
		return
	}
	username, ok := b.requireSecret(ctx, mid, scope, "datalink_username", out)
	if !ok {
		return
	}
	password, ok := b.requireSecret(ctx, mid, scope, "datalink_password", out)
	if !ok {
		return
	}
	dl := ccbill.NewDataLinkClient(&config.CCBillConfig{
		ClientAccNum:     strings.TrimSpace(acc),
		ClientSubAcc:     strings.TrimSpace(sub),
		DataLinkUsername: username,
		DataLinkPassword: password,
		Sandbox:          scope.Environment == config.EnvironmentSandbox,
	})
	dl.ReadOnly = b.readOnly()
	if b.Endpoints.CCBillDataLinkBaseURL != "" {
		dl.BaseURL = b.Endpoints.CCBillDataLinkBaseURL
		dl.LoopbackFixture = true
	}
	out.Fetchers[ProviderCCBill] = keyedFetcher{RailFetcher: NewCCBillFetcher(dl), key: scope.AccountID}
	out.Probers[ProviderCCBill] = &CCBillSubscriptionProber{Client: dl}
}

func (b MerchantFetcherBuilder) buildStripe(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, out *MerchantPullClients) {
	secretKey, ok := b.requireSecret(ctx, mid, scope, "secret_key", out)
	if !ok {
		return
	}
	fetcher := NewStripeFetcher(secretKey)
	fetcher.HTTPClient = b.StripeClients.ReadOnlyClient(30 * time.Second)
	fetcher.BaseURL = b.Endpoints.StripeBaseURL
	out.Fetchers[ProviderStripe] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
	out.Probers[ProviderStripe] = &StripeSubscriptionProber{Prober: &subscriptions.HTTPStripeLivenessProber{
		SecretKey:  secretKey,
		HTTPClient: b.StripeClients.ReadOnlyClient(30 * time.Second),
		BaseURL:    b.Endpoints.StripeBaseURL,
	}}
}

// buildSolana: Solana pulls read public chain state, so the rail needs no pull
// credential. A declared Solana PSP arms the fetcher on its network: its
// rpc_url first, the network's public endpoint as fallback.
func (b MerchantFetcherBuilder) buildSolana(ctx context.Context, mid billing.MerchantID, scope merchants.PSPScope, out *MerchantPullClients) {
	if b.DB == nil {
		return
	}
	rpcURL, err := b.Merchants.OptionalPSPSecret(ctx, mid, scope, config.SolanaSecretRPCURL)
	if err != nil {
		b.warnScopeError(ctx, mid, ProviderSolana, err)
		return
	}
	// A sandbox run's loopback RPC replaces the public one here as it does
	// for the runtime's own Solana clients (hermetic CI never reaches devnet).
	rpc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
		Network:   scope.Environment,
		URL:       rpcURL,
		PublicURL: config.SandboxSolanaRPCURL(b.Config),
		ReadOnly:  true,
	})
	fetcher := NewSolanaFetcher(rpc, SolanaSubscriptionSourceFromDB(b.DB))
	// The PSP's treasury is the wallet the discovery lanes scan.
	fetcher.MerchantWallet = scope.AccountID
	plans := SolanaPlanSourceFromDB(b.DB)
	fetcher.Plans = func(ctx context.Context) ([]string, error) { return plans(db.WithPSPID(ctx, scope.ID)) }
	fetcher.Due = SolanaDueSubscriptionSourceFromDB(b.DB) // due-window bulk-fetch filter
	fetcher.Resolve = SolanaLocalRecordResolverFromDB(b.DB)
	out.Fetchers[ProviderSolana] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
}
