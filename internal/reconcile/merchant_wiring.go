package reconcile

import (
	"context"
	"errors"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699: the pull plane (provider refresh, unknown-cohort resolution, per-sub
// probes, operator pulls) arms PER MERCHANT from the merchant-secrets store.
//
// Two credential planes exist post-#653/#667 and this file defines their ONE
// precedence rule: MERCHANT-STORE FIRST, BOOT-CONFIG RAILS AS FALLBACK.
//
//   - A merchant that declares a rail account row (rail_merchant_accounts, the
//     manifest-seeded catalog) resolves that rail from the store ONLY. Missing
//     or incomplete secrets fail LOUD: the rail's fetcher/prober is simply
//     absent and ONE WARN names the merchant, rail and missing secret — never a
//     silent empty map, never a cross-plane fallback (mirrors the checkout
//     write path's fail-closed rule).
//   - A rail with no store row falls back to the boot-config rail set
//     (embedded Options.PaymentProviders / standalone overrides).
//   - Both planes armed with DIFFERING credentials → the store wins, with a
//     WARN naming the conflict.
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
}

// ProviderEndpoints overrides provider base URLs on store-armed clients — a
// test seam for fake provider HTTP servers. Zero value = real endpoints.
type ProviderEndpoints struct {
	NMIQueryURL           string
	NMIV5BaseURL          string
	CCBillDataLinkBaseURL string
	StripeBaseURL         string
}

// MerchantFetcherBuilder builds one merchant's fetchers/probers at use time.
// Merchants is the store plane (nil = boot plane only); Rails plus the
// boot-built clients are the fallback plane.
type MerchantFetcherBuilder struct {
	Config    *config.Config
	Rails     config.RailMerchantAccountSet
	Merchants *merchants.Service
	DB        *db.DB

	// Boot-built fallback clients (the pre-#699 plane).
	NMIClients     map[string]*nmi.NMIClient
	CCBillDataLink *ccbill.DataLinkClient
	SolanaRPC      *solanaint.RPCClient

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

// environment is the deployment's provider-account environment: test under
// test_mode, live otherwise (#681) — the test_mode credential filter.
func (b MerchantFetcherBuilder) environment() string {
	return config.ExpectedProviderEnvironment(b.testMode())
}

// resolveScope resolves the merchant's declared account row for a rail from
// the store plane: the pinned account when AccountIDs names one, else the pull
// scope (active for new work, else newest archived for drain — #655). ok=false
// means the merchant declares NO account on the rail → boot-config fallback.
func (b MerchantFetcherBuilder) resolveScope(ctx context.Context, mid merchant.ID, provider Provider) (merchants.RailMerchantAccountScope, bool) {
	if b.Merchants == nil {
		return merchants.RailMerchantAccountScope{}, false
	}
	rail := string(provider)
	if pin := strings.TrimSpace(b.AccountIDs[provider]); pin != "" {
		scope, ok, err := b.Merchants.RailMerchantAccountScopeByAccountID(ctx, mid, rail, pin)
		if err != nil {
			b.warnScopeError(ctx, mid, provider, err)
			return merchants.RailMerchantAccountScope{}, false
		}
		if !ok {
			log.WithContext(ctx).WithFields(log.Fields{
				"merchant_id": mid.String(), "rail": rail, "account_id": pin,
			}).Warn("provider pull: pinned provider account is not declared for this merchant")
		}
		return scope, ok
	}
	scope, ok, err := b.Merchants.PullRailMerchantAccountScope(ctx, mid, rail, b.environment())
	if err != nil {
		b.warnScopeError(ctx, mid, provider, err)
		return merchants.RailMerchantAccountScope{}, false
	}
	return scope, ok
}

func (b MerchantFetcherBuilder) warnScopeError(ctx context.Context, mid merchant.ID, provider Provider, err error) {
	log.WithContext(ctx).WithError(err).WithFields(log.Fields{
		"merchant_id": mid.String(), "rail": string(provider),
	}).Warn("provider pull: rail not armed — provider-account resolution failed")
}

// secret loads one scoped secret. found=false with nil err means the secret is
// genuinely absent (terminal for this pass); backend errors surface as err.
func (b MerchantFetcherBuilder) secret(ctx context.Context, mid merchant.ID, scope merchants.RailMerchantAccountScope, key string) (string, bool, error) {
	if b.Merchants == nil || b.Merchants.Secrets() == nil {
		return "", false, nil
	}
	name, err := merchants.RailMerchantAccountSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
	if err != nil {
		return "", false, err
	}
	sec, err := b.Merchants.Secrets().Get(ctx, mid, name)
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
func (b MerchantFetcherBuilder) requireSecret(ctx context.Context, mid merchant.ID, scope merchants.RailMerchantAccountScope, key string) (string, bool) {
	value, found, err := b.secret(ctx, mid, scope, key)
	if err != nil {
		name, _ := merchants.RailMerchantAccountSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant secret backend failed (retried next pass)")
		return "", false
	}
	if !found {
		name, _ := merchants.RailMerchantAccountSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
		log.WithContext(ctx).WithFields(log.Fields{
			"merchant_id": mid.String(), "rail": scope.Rail, "secret": name,
		}).Warn("provider pull: rail not armed — merchant secret missing (#699)")
		return "", false
	}
	return value, true
}

func (b MerchantFetcherBuilder) warnStoreOverridesBoot(ctx context.Context, mid merchant.ID, provider Provider) {
	log.WithContext(ctx).WithFields(log.Fields{
		"merchant_id": mid.String(), "rail": string(provider),
	}).Warn("provider pull: merchant-store credentials differ from boot-config rails; store wins (#699)")
}

func (b MerchantFetcherBuilder) buildNMI(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScope(ctx, mid, ProviderNMI); ok {
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
		if _, boot, _ := selectNMIClient(b.Rails, b.NMIClients, ""); boot != nil && boot.SecurityKey != "" && boot.SecurityKey != securityKey {
			b.warnStoreOverridesBoot(ctx, mid, ProviderNMI)
		}
		out.Fetchers[ProviderNMI] = keyedFetcher{RailFetcher: NewNMIFetcher(client), key: scope.AccountID}
		out.Probers[ProviderNMI] = &NMISubscriptionProber{Client: client}
		return
	}
	// Boot-config fallback: no declared account on this rail.
	key, c, err := selectNMIClient(b.Rails, b.NMIClients, "")
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).
			Warn("provider pull: boot-config NMI rail selection failed")
		return
	}
	if c == nil {
		return
	}
	out.Fetchers[ProviderNMI] = keyedFetcher{RailFetcher: NewNMIFetcher(c), key: key}
	out.Probers[ProviderNMI] = &NMISubscriptionProber{Client: c}
}

func (b MerchantFetcherBuilder) buildCCBill(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScope(ctx, mid, ProviderCCBill); ok {
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
		if boot := b.CCBillDataLink; boot != nil &&
			(boot.Username != username || boot.Password != password || boot.ClientAccNum != dl.ClientAccNum || boot.ClientSubAcc != dl.ClientSubAcc) {
			b.warnStoreOverridesBoot(ctx, mid, ProviderCCBill)
		}
		out.Fetchers[ProviderCCBill] = keyedFetcher{RailFetcher: NewCCBillFetcher(dl), key: scope.AccountID}
		out.Probers[ProviderCCBill] = &CCBillSubscriptionProber{Client: dl}
		out.CCBillDataLink = dl
		return
	}
	// Boot-config fallback. The DataLink bulk lane keeps the boot client even
	// without a declared rail entry (pre-#699 refresh-job behavior).
	out.CCBillDataLink = b.CCBillDataLink
	if b.CCBillDataLink == nil {
		return
	}
	out.Probers[ProviderCCBill] = &CCBillSubscriptionProber{Client: b.CCBillDataLink}
	key, proc, err := selectRailByType(b.Rails, models.RailCCBill, "")
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).
			Warn("provider pull: boot-config CCBill rail selection failed")
		return
	}
	if proc == nil {
		return
	}
	out.Fetchers[ProviderCCBill] = keyedFetcher{RailFetcher: NewCCBillFetcher(b.CCBillDataLink), key: key}
}

func (b MerchantFetcherBuilder) buildStripe(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if scope, ok := b.resolveScope(ctx, mid, ProviderStripe); ok {
		secretKey, ok := b.requireSecret(ctx, mid, scope, "secret_key")
		if !ok {
			return
		}
		if _, boot, _ := selectRailByType(b.Rails, models.RailStripe, ""); boot != nil && boot.Stripe != nil &&
			boot.Stripe.SecretKey != "" && boot.Stripe.SecretKey != secretKey {
			b.warnStoreOverridesBoot(ctx, mid, ProviderStripe)
		}
		fetcher := NewStripeFetcher(secretKey)
		fetcher.BaseURL = b.Endpoints.StripeBaseURL
		out.Fetchers[ProviderStripe] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
		out.Probers[ProviderStripe] = &StripeSubscriptionProber{Prober: &subscriptions.HTTPStripeLivenessProber{
			SecretKey: secretKey,
			BaseURL:   b.Endpoints.StripeBaseURL,
		}}
		return
	}
	// Boot-config fallback.
	key, proc, err := selectRailByType(b.Rails, models.RailStripe, "")
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).
			Warn("provider pull: boot-config Stripe rail selection failed")
		return
	}
	if proc == nil || proc.Stripe == nil || proc.Stripe.SecretKey == "" {
		return
	}
	out.Fetchers[ProviderStripe] = keyedFetcher{RailFetcher: NewStripeFetcher(proc.Stripe.SecretKey), key: key}
	if p, err := subscriptions.NewStripeLivenessProber(b.Rails); err == nil && p != nil {
		out.Probers[ProviderStripe] = &StripeSubscriptionProber{Prober: p}
	}
}

// buildSolana: Solana pulls read public chain state — the rail holds no
// per-merchant PULL credential (private_key is the operator-only SIGNING key).
// A declared solana account row arms the fetcher using the deployment RPC
// client when one was boot-built, else a read-only default (public RPC
// fallback, network derived from test_mode — both declared config defaults).
func (b MerchantFetcherBuilder) buildSolana(ctx context.Context, mid merchant.ID, out *MerchantPullClients) {
	if b.DB == nil {
		return
	}
	if scope, ok := b.resolveScope(ctx, mid, ProviderSolana); ok {
		rpc := b.SolanaRPC
		// #711: the account settings block carries the merchant's RPC knobs
		// (rpc_provider / rpc_api_key); store settings win over the boot client (#699).
		settings, err := config.ParseSolanaAccountSettings(scope.Settings)
		if err != nil {
			b.warnScopeError(ctx, mid, ProviderSolana, err)
			return
		}
		if rpc == nil || settings.RPCProvider != "" || settings.RPCAPIKey != "" {
			network := "mainnet"
			if b.testMode() {
				network = "devnet"
			}
			rpc = solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
				RPCProvider: settings.RPCProvider,
				RPCAPIKey:   settings.RPCAPIKey,
				Network:     network,
				ReadOnly:    true,
			})
		}
		fetcher := NewSolanaFetcher(rpc, SolanaSubscriptionSourceFromDB(b.DB))
		// #714 discovery lanes: the declared account_id IS the merchant wallet.
		fetcher.MerchantWallet = scope.AccountID
		fetcher.Plans = SolanaPlanSourceFromDB(b.DB)
		fetcher.Due = SolanaDueSubscriptionSourceFromDB(b.DB) // #720: due-window bulk-fetch filter
		fetcher.Resolve = SolanaLocalRecordResolverFromDB(b.DB)
		out.Fetchers[ProviderSolana] = keyedFetcher{RailFetcher: fetcher, key: scope.AccountID}
		return
	}
	// Boot-config fallback.
	if b.SolanaRPC == nil {
		return
	}
	key, proc, err := selectRailByType(b.Rails, models.RailSolana, "")
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).
			Warn("provider pull: boot-config Solana rail selection failed")
		return
	}
	if proc == nil {
		return
	}
	fetcher := NewSolanaFetcher(b.SolanaRPC, SolanaSubscriptionSourceFromDB(b.DB))
	// Boot-config fallback declares no account_id, so the wallet scan stays
	// disarmed unless an operator pull passes one; enumeration + resolution
	// still arm from the DB.
	fetcher.Plans = SolanaPlanSourceFromDB(b.DB)
	fetcher.Due = SolanaDueSubscriptionSourceFromDB(b.DB) // #720: due-window bulk-fetch filter
	fetcher.Resolve = SolanaLocalRecordResolverFromDB(b.DB)
	out.Fetchers[ProviderSolana] = keyedFetcher{RailFetcher: fetcher, key: key}
}
