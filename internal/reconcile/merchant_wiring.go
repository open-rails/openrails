package reconcile

import (
	"context"
	"errors"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699/#788: the pull plane (provider refresh, unknown-cohort resolution,
// per-sub probes, operator pulls) arms PER MERCHANT from the armed rail state
// (psps rows + the merchant secret store) — the ONE Layer-C
// resolution seam. There is no boot-config plane anymore: a rail with no
// declared account row is simply not armed. Missing or incomplete secrets
// fail LOUD: the rail's fetcher/prober is absent and ONE WARN names the
// merchant, rail and missing secret — never a silent empty map, never a
// default-allow.
//
// Secrets are resolved per merchant, at use time, inside the merchant scope.
// Nothing is cached process-wide (#653: never a plaintext all-merchants tree);
// clients are cheap per-merchant structs rebuilt on every pass.

// MerchantPullClients is one merchant's armed pull plane.
type MerchantPullClients struct {
	Fetchers map[Provider]RailFetcher
	Probers  map[Provider]SubscriptionProber
	// CCBillDataLink feeds the CCBill DataLink bulk lane (the ACTIVEMEMBERS
	// roster reconcile) with the same per-merchant client the fetcher uses.
	CCBillDataLink *ccbill.DataLinkClient
	// Coverage (#841) records, per armed rail, how many PSPs the merchant
	// declares ACTIVE on it versus how many this pass actually read. A pull
	// arms from exactly ONE PSP, so a merchant running two NMI PSPs has half
	// its book invisible to the roster — and "absent from an exhaustive roster"
	// would cancel every subscription of the PSP that was not read.
	Coverage map[Provider]PSPCoverage
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
	Config    *config.Config
	Merchants *merchants.Service
	DB        *db.DB

	// AccountIDs optionally pins a rail to one declared account_id (operator
	// pulls; may target archived accounts for drain, #655).
	AccountIDs map[Provider]string

	Endpoints ProviderEndpoints
}

// Build resolves the merchant's pull credentials and returns the armed plane.
// It never fails as a whole: a rail that cannot arm is absent (with its WARN
// already logged) so the remaining rails keep pulling.
func (b MerchantFetcherBuilder) Build(ctx context.Context, mid merchant.ID) MerchantPullClients {
	out := MerchantPullClients{
		Fetchers: map[Provider]RailFetcher{},
		Probers:  map[Provider]SubscriptionProber{},
		Coverage: map[Provider]PSPCoverage{},
	}
	b.buildNMI(ctx, mid, &out)
	b.buildCCBill(ctx, mid, &out)
	b.buildStripe(ctx, mid, &out)
	b.buildSolana(ctx, mid, &out)
	return out
}

func (b MerchantFetcherBuilder) testMode() bool {
	return b.Config != nil && b.Config.IsTestMode()
}

func (b MerchantFetcherBuilder) readOnly() bool {
	return b.Config != nil && b.Config.IsProviderReadOnly()
}

// environment is the deployment's PSP environment: test under
// test_mode, live otherwise (#681) — the test_mode credential filter.
func (b MerchantFetcherBuilder) environment() string {
	return config.ExpectedProviderEnvironment(b.testMode())
}

// resolveScopeCoverage is resolveScope plus the #841 coverage record: how many
// PSPs the merchant declares active on the rail versus the one this pass arms
// from.
func (b MerchantFetcherBuilder) resolveScopeCoverage(ctx context.Context, mid merchant.ID, provider Provider, out *MerchantPullClients) (merchants.PSPScope, bool) {
	scope, ok := b.resolveScopeInner(ctx, mid, provider)
	if !ok || out == nil {
		return scope, ok
	}
	declared := 1
	if b.Merchants != nil {
		if n, err := b.Merchants.CountActivePSPsForRail(ctx, mid, string(provider), b.environment()); err != nil {
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
		}).Warn("provider pull: rail has multiple active PSPs but a pull arms from ONE — its roster cannot prove absence for the siblings it did not read (#841)")
	}
	out.Coverage[provider] = PSPCoverage{
		Declared: declared,
		Pulled:   1,
		Binding:  PSPBinding{ID: scope.ID, Rail: scope.Rail, AccountID: scope.AccountID},
	}
	return scope, true
}

func (b MerchantFetcherBuilder) resolveScopeInner(ctx context.Context, mid merchant.ID, provider Provider) (merchants.PSPScope, bool) {
	if b.Merchants == nil {
		return merchants.PSPScope{}, false
	}
	rail := string(provider)
	if pin := strings.TrimSpace(b.AccountIDs[provider]); pin != "" {
		scope, ok, err := b.Merchants.PSPScopeByAccountID(ctx, mid, rail, pin)
		if err != nil {
			b.warnScopeError(ctx, mid, provider, err)
			return merchants.PSPScope{}, false
		}
		if !ok {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": rail, "account_id": pin,
			}).Warn("provider pull: pinned PSP is not declared for this merchant")
		}
		return scope, ok
	}
	scope, ok, err := b.Merchants.PullPSPScope(ctx, mid, rail, b.environment())
	if err != nil {
		b.warnScopeError(ctx, mid, provider, err)
		return merchants.PSPScope{}, false
	}
	return scope, ok
}

func (b MerchantFetcherBuilder) warnScopeError(ctx context.Context, mid merchant.ID, provider Provider, err error) {
	log.WithContext(ctx).WithError(err).WithFields(log.Fields{
		"merchant_id": mid.String(), "rail": string(provider),
	}).Warn("provider pull: rail not armed — PSP resolution failed")
}

// secret loads one scoped secret. found=false with nil err means the secret is
// genuinely absent (terminal for this pass); backend errors surface as err.
func (b MerchantFetcherBuilder) secret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, bool, error) {
	if b.Merchants == nil || b.Merchants.Secrets() == nil {
		return "", false, nil
	}
	// or#812: honour the PSP row's rotation version floor.
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

// requireSecret is secret plus the #699 fail-loud contract: absence or a
// backend failure logs ONE WARN naming merchant, rail and the secret name.
func (b MerchantFetcherBuilder) requireSecret(ctx context.Context, mid merchant.ID, scope merchants.PSPScope, key string) (string, bool) {
	value, found, err := b.secret(ctx, mid, scope, key)
	if err != nil {
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant secret backend failed (retried next pass)")
		return "", false
	}
	if !found {
		name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant secret missing (#699)")
		return "", false
	}
	return value, true
}

func (b MerchantFetcherBuilder) buildNMI(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScopeCoverage(ctx, mid, ProviderNMI, out); ok {
		securityKey, ok := b.requireSecret(ctx, mid, scope, "security_key")
		if !ok {
			return // fail closed: a declared account never falls back across planes
		}
		// Optional on the pull plane; loaded so the client mirrors the real
		// account posture instead of a fabricated empty.
		webhookSecret, _, _ := b.secret(ctx, mid, scope, "webhook_signing_secret")
		client, err := nmi.NewClient(scope.AccountID, &config.NMIProviderSettings{
			SecurityKey:   securityKey,
			WebhookSecret: webhookSecret,
		}, b.testMode())
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": "nmi",
			}).Warn("provider pull: rail not armed — NMI client build failed")
			return
		}
		client.ReadOnly = b.readOnly()
		if b.Endpoints.NMIQueryURL != "" {
			client.QueryURL = b.Endpoints.NMIQueryURL
		}
		if b.Endpoints.NMIV5BaseURL != "" {
			client.V5BaseURL = b.Endpoints.NMIV5BaseURL
		}
		out.Fetchers[ProviderNMI] = keyedFetcher{RailFetcher: NewNMIFetcher(client), key: scope.AccountID}
		out.Probers[ProviderNMI] = &NMISubscriptionProber{Client: client}
	}
}

func (b MerchantFetcherBuilder) buildCCBill(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScopeCoverage(ctx, mid, ProviderCCBill, out); ok {
		// #697: CCBill account_id is dash-joined (clientAccnum-clientSubacc,
		// e.g. 945280-0000). Both parts are numeric, so the first dash splits.
		acc, sub, cut := strings.Cut(strings.TrimSpace(scope.AccountID), "-")
		if !cut || strings.TrimSpace(acc) == "" || strings.TrimSpace(sub) == "" {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": "ccbill", "account_id": scope.AccountID,
			}).Warn("provider pull: rail not armed — CCBill account_id must be clientAccnum-clientSubacc (e.g. 945280-0000)")
			return
		}
		username, ok := b.requireSecret(ctx, mid, scope, "datalink_username")
		if !ok {
			return
		}
		password, ok := b.requireSecret(ctx, mid, scope, "datalink_password")
		if !ok {
			return
		}
		dl := ccbill.NewDataLinkClient(&config.CCBillConfig{
			ClientAccNum:     strings.TrimSpace(acc),
			ClientSubAcc:     strings.TrimSpace(sub),
			DataLinkUsername: username,
			DataLinkPassword: password,
			TestMode:         b.testMode(),
		})
		dl.ReadOnly = b.readOnly()
		if b.Endpoints.CCBillDataLinkBaseURL != "" {
			dl.BaseURL = b.Endpoints.CCBillDataLinkBaseURL
		}
		out.Fetchers[ProviderCCBill] = keyedFetcher{RailFetcher: NewCCBillFetcher(dl), key: scope.AccountID}
		out.Probers[ProviderCCBill] = &CCBillSubscriptionProber{Client: dl}
		out.CCBillDataLink = dl
	}
}

func (b MerchantFetcherBuilder) buildStripe(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScopeCoverage(ctx, mid, ProviderStripe, out); ok {
		secretKey, ok := b.requireSecret(ctx, mid, scope, "secret_key")
		if !ok {
			return
		}
		fetcher := NewStripeFetcher(secretKey)
		fetcher.BaseURL = b.Endpoints.StripeBaseURL
		out.Fetchers[ProviderStripe] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
		out.Probers[ProviderStripe] = &StripeSubscriptionProber{Prober: &subscriptions.HTTPStripeLivenessProber{
			SecretKey: secretKey,
			BaseURL:   b.Endpoints.StripeBaseURL,
		}}
	}
}

// buildSolana: Solana pulls read public chain state — the rail holds no
// per-merchant PULL credential (private_key is the operator-only SIGNING key).
// A declared solana account row arms the fetcher; its settings block carries
// the merchant's RPC knobs (rpc_provider / rpc_api_key, #711), defaulting to
// the public RPC fallback with the network derived from test_mode.
func (b MerchantFetcherBuilder) buildSolana(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if b.DB == nil {
		return
	}
	if scope, ok := b.resolveScopeCoverage(ctx, mid, ProviderSolana, out); ok {
		settings, err := config.ParseSolanaAccountSettings(scope.Settings)
		if err != nil {
			b.warnScopeError(ctx, mid, ProviderSolana, err)
			return
		}
		network := "mainnet"
		if b.testMode() {
			network = "devnet"
		}
		rpc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
			RPCProvider: settings.RPCProvider,
			RPCAPIKey:   settings.RPCAPIKey,
			Network:     network,
			ReadOnly:    true,
		})
		fetcher := NewSolanaFetcher(rpc, SolanaSubscriptionSourceFromDB(b.DB))
		// #714 discovery lanes: the declared account_id IS the merchant wallet.
		fetcher.MerchantWallet = scope.AccountID
		fetcher.Plans = SolanaPlanSourceFromDB(b.DB)
		fetcher.Due = SolanaDueSubscriptionSourceFromDB(b.DB) // #720: due-window bulk-fetch filter
		fetcher.Resolve = SolanaLocalRecordResolverFromDB(b.DB)
		out.Fetchers[ProviderSolana] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
	}
}
