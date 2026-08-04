package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #725/#788: the arrears/top-up collection plane arms rail credentials PER
// MERCHANT from the merchant-secrets store AT CHARGE TIME — the ONE Layer-C
// resolution seam (there is no boot-config plane anymore, #788). A merchant
// that declares a rail account resolves that rail from the store ONLY —
// missing or unreadable secrets fail the charge CLOSED (loud error naming
// merchant, rail and secret). A rail with no store row simply has no
// collection adapter (the charge fails closed with "no invoice collection
// adapter configured"). Nothing is cached: adapters are cheap per-charge
// structs, so a rotated credential takes effect on the next charge — same
// posture as checkout, webhooks and the pull plane.

// CollectionAdapterResolver arms the store-scoped adapter for one saved-method
// charge. ok=false with nil err = the merchant declares no account on the rail
// (nothing to charge with — the charge fails closed, there is no fallback
// plane); err = declared but not armable (fail closed).
type CollectionAdapterResolver interface {
	ResolveCollectionAdapter(ctx context.Context, method gen.OpenrailsPaymentMethod) (CollectionAdapter, bool, error)
}

// CollectionEndpoints overrides provider base URLs on store-armed collection
// clients — a test seam for fake provider HTTP servers. Zero value = real.
type CollectionEndpoints struct {
	StripeBaseURL string
	NMIV5BaseURL  string
	// Classic NMI endpoints (#727 manual-rebill leg: Direct Post + Query API).
	NMIDirectPostURL string
	NMIQueryURL      string
	// BTBaseURL points store-armed Basis Theory clients at a fake server (#795).
	BTBaseURL string
}

// CollectionPlane is the runtime-facing per-merchant credential resolver
// surface (#725/#788): store-armed collection adapters plus raw NMI clients,
// plus the #828 in-doubt-charge provider read.
// Satisfied by *MerchantCollectionAdapterBuilder; tests may inject fakes.
type CollectionPlane interface {
	CollectionAdapterResolver
	NMIClientResolver
	CollectionVerifier
}

// NMIClientResolver is the raw-client NMI leg of the #725 store resolver, for
// charge paths that need the client itself (manual dunning rebills) rather
// than a CollectionAdapter. Same contract: ok=false with nil err = merchant
// declares no NMI account (fails closed — no fallback plane); err = declared
// but not armable (fail closed).
type NMIClientResolver interface {
	ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error)
}

// MerchantCollectionAdapterBuilder builds one merchant's collection adapter at
// charge time. MerchantsFn is late-bound (Runtime.Merchants is wired after the
// charger is constructed); a nil fn/service = boot plane only (#699).
type MerchantCollectionAdapterBuilder struct {
	Config      *config.Config
	DB          *db.DB
	MerchantsFn func() *merchants.Service
	Endpoints   CollectionEndpoints
}

var _ CollectionAdapterResolver = (*MerchantCollectionAdapterBuilder)(nil)
var _ NMIClientResolver = (*MerchantCollectionAdapterBuilder)(nil)

func (b *MerchantCollectionAdapterBuilder) merchants() *merchants.Service {
	if b == nil || b.MerchantsFn == nil {
		return nil
	}
	return b.MerchantsFn()
}

func (b *MerchantCollectionAdapterBuilder) testMode() bool {
	return b.Config != nil && b.Config.IsTestMode()
}

// environment is the deployment's provider-account environment (#681).
func (b *MerchantCollectionAdapterBuilder) environment() string {
	return config.ExpectedProviderEnvironment(b.testMode())
}

func (b *MerchantCollectionAdapterBuilder) ResolveCollectionAdapter(ctx context.Context, method gen.OpenrailsPaymentMethod) (CollectionAdapter, bool, error) {
	svc := b.merchants()
	if svc == nil || b.DB == nil {
		return nil, false, nil
	}
	rail := normalizeRail(method.Rail)
	isStripe := rail == string(models.RailStripe)
	if !isStripe && !rails.IsNMI(models.Rail(rail)) {
		return nil, false, nil // rail has no store-armable collection adapter
	}
	mid := merchant.ID(method.MerchantID)
	scope, ok, err := b.resolveScope(ctx, svc, mid, rail, method.PspID)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil // no declared account → nothing armable for this rail
	}
	var adapter CollectionAdapter
	switch {
	case isStripe:
		adapter, err = b.stripeAdapter(ctx, svc, mid, scope)
	case method.Custodian == models.CustodianBasisTheory:
		// or#879: same rail, same gateway, different transport — the card is
		// held by the custodian, so the charge goes through its proxy. The
		// INSTRUMENT decides this, not the rail.
		adapter, err = b.custodianProxyAdapter(ctx, svc, mid, scope)
	default:
		adapter, err = b.nmiAdapter(ctx, svc, mid, scope)
	}
	if err != nil {
		return nil, false, err
	}
	return adapter, true, nil
}

// resolveScope picks the account the charge settles through: the stamped #704
// provenance account when present (archived stays addressable for existing
// obligations, #655), else the pull scope (active for new work, else newest
// archived for drain — the #699 resolver).
func (b *MerchantCollectionAdapterBuilder) resolveScope(ctx context.Context, svc *merchants.Service, mid merchant.ID, rail string, stamped *uuid.UUID) (merchants.PSPScope, bool, error) {
	if stamped != nil {
		row, err := b.DB.Gen(ctx).GetPSP(ctx, *stamped)
		if err != nil {
			return merchants.PSPScope{}, false, fmt.Errorf("load stamped provider account: %w", err)
		}
		if !rails.SameRail(models.Rail(row.Rail), models.Rail(rail)) {
			return merchants.PSPScope{}, false, fmt.Errorf("stamped provider account %s is on rail %s, not %s", row.ID, row.Rail, rail)
		}
		return merchants.PSPScope{
			ID: row.ID, Rail: row.Rail, Environment: row.Environment, AccountID: row.AccountID,
		}, true, nil
	}
	return svc.PullRailMerchantAccountScope(ctx, mid, rail, b.environment())
}

// ResolveNMIClient arms the store-scoped NMI client for one charge (#727 —
// the manual-rebill leg of the ONE #725 resolver). Scope pick: the stamped
// provenance account when present (dunning stamps the subscription's account
// on the intent), else the NMI pull scope. Nil-receiver-safe: nil builder =
// boot plane only.
func (b *MerchantCollectionAdapterBuilder) ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if b == nil {
		return nil, false, nil
	}
	svc := b.merchants()
	if svc == nil || b.DB == nil {
		return nil, false, nil
	}
	mid := merchant.ID(merchantID)
	scope, ok, err := b.resolveScope(ctx, svc, mid, string(models.RailNMI), stampedAccountID)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil // no declared NMI account → nothing armable
	}
	client, err := b.nmiClient(ctx, svc, mid, scope)
	if err != nil {
		return nil, false, err
	}
	return client, true, nil
}

// VerifyCollectionCharge (#828) is the invoice-side ambiguity resolver's
// provider READ: for NMI-family rails it arms the store-scoped client and
// asks the Query API for a successful sale carrying the wire order reference
// — the same probe the manual-rebill intent verifier trusts for its
// no-double-charge invariant. Custodian-proxied charges settle on the SAME NMI
// gateway account (or#879), so one branch covers both transports. Rails
// without a usable read (Stripe relies on its own request idempotency and
// cannot park ambiguous mid-transport) report Supported=false.
func (b *MerchantCollectionAdapterBuilder) VerifyCollectionCharge(ctx context.Context, method gen.OpenrailsPaymentMethod, wireOrderRef string) (CollectionVerifyResult, error) {
	if b == nil {
		return CollectionVerifyResult{}, nil
	}
	svc := b.merchants()
	if svc == nil || b.DB == nil {
		return CollectionVerifyResult{}, nil
	}
	rail := normalizeRail(method.Rail)
	if !rails.IsNMI(models.Rail(rail)) {
		return CollectionVerifyResult{}, nil
	}
	client, ok, err := b.ResolveNMIClient(ctx, method.MerchantID, method.PspID)
	if err != nil {
		return CollectionVerifyResult{}, err
	}
	if !ok {
		return CollectionVerifyResult{}, nil
	}
	txnID, found, err := client.FindSuccessfulSaleByOrderID(ctx, wireOrderRef)
	if err != nil {
		return CollectionVerifyResult{}, fmt.Errorf("nmi query for order ref %q: %w", wireOrderRef, err)
	}
	return CollectionVerifyResult{Supported: true, Settled: found, TransactionID: txnID}, nil
}

func (b *MerchantCollectionAdapterBuilder) stripeAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	secretKey, err := b.requireSecret(ctx, svc, mid, scope, "secret_key")
	if err != nil {
		return nil, err
	}
	service := &subscriptions.StripeService{
		Config: b.Config,
		// The store-resolved account IS the source here; hand StripeService a
		// fixed single-account view so its ctx-time resolution can't drift
		// from the scope this adapter was armed for.
		Rails: railresolve.FixedSet{
			string(models.RailStripe): {
				Rail:      models.RailStripe,
				AccountID: scope.AccountID,
				Stripe:    &config.StripeRailConfig{SecretKey: secretKey},
			},
		},
	}
	if b.Endpoints.StripeBaseURL != "" {
		service.SetBaseURLForTest(b.Endpoints.StripeBaseURL)
	}
	return NewStripeCollectionAdapter(b.DB, service), nil
}

// custodianProxyAdapter arms the #795 detokenizing-proxy collection adapter:
// the custodian's private app key and THIS PSP's own gateway security key —
// one PSP row, both halves (or#879 folded the old cross-account pointer away).
func (b *MerchantCollectionAdapterBuilder) custodianProxyAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	custody, err := config.ParseCustodySettings(scope.Settings)
	if err != nil {
		return nil, fmt.Errorf("psp %s/%s: %w", scope.Rail, scope.AccountID, err)
	}
	if !custody.ThirdParty() {
		return nil, fmt.Errorf("psp %s/%s: instrument is held by custodian %s but the PSP declares none", scope.Rail, scope.AccountID, models.CustodianBasisTheory)
	}
	apiKey, err := b.requireSecret(ctx, svc, mid, scope, config.CustodianSecretAPIKey)
	if err != nil {
		return nil, err
	}
	gatewayKey, err := b.requireSecret(ctx, svc, mid, scope, "security_key")
	if err != nil {
		return nil, err
	}
	bt, err := basistheory.New(basistheory.Config{
		APIKey:   apiKey,
		BaseURL:  b.Endpoints.BTBaseURL,
		ReadOnly: b.Config != nil && b.Config.IsProviderReadOnly(),
	})
	if err != nil {
		return nil, fmt.Errorf("build store-armed BT client: %w", err)
	}
	gw := nmiproxy.GatewayConfig{SecurityKey: gatewayKey, DirectPostURL: b.Endpoints.NMIDirectPostURL}
	return NewCustodianProxyCollectionAdapter(nmiproxy.New(bt, gw)), nil
}

func (b *MerchantCollectionAdapterBuilder) nmiAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	client, err := b.nmiClient(ctx, svc, mid, scope)
	if err != nil {
		return nil, err
	}
	return NewNMICollectionAdapter(client), nil
}

// nmiClient builds the store-armed NMI client for scope — shared by the
// collection adapter and the #727 manual-rebill leg.
func (b *MerchantCollectionAdapterBuilder) nmiClient(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	securityKey, err := b.requireSecret(ctx, svc, mid, scope, "security_key")
	if err != nil {
		return nil, err
	}
	// Optional on the charge plane; loaded so the client mirrors the real
	// account posture instead of a fabricated empty.
	webhookSecret, _, _ := b.secret(ctx, svc, mid, scope, "webhook_signing_secret")
	client, err := nmi.NewClient(scope.AccountID, &config.NMIProviderSettings{
		SecurityKey:   securityKey,
		WebhookSecret: webhookSecret,
	}, b.testMode())
	if err != nil {
		return nil, fmt.Errorf("build store-armed NMI client: %w", err)
	}
	client.ReadOnly = b.Config != nil && b.Config.IsProviderReadOnly()
	if b.Endpoints.NMIV5BaseURL != "" {
		client.V5BaseURL = b.Endpoints.NMIV5BaseURL
	}
	if b.Endpoints.NMIDirectPostURL != "" {
		client.DirectPostURL = b.Endpoints.NMIDirectPostURL
	}
	if b.Endpoints.NMIQueryURL != "" {
		client.QueryURL = b.Endpoints.NMIQueryURL
	}
	return client, nil
}

// secret loads one scoped secret. found=false with nil err means the secret is
// genuinely absent; backend errors surface as err.
func (b *MerchantCollectionAdapterBuilder) secret(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope, key string) (string, bool, error) {
	if svc.Secrets() == nil {
		return "", false, nil
	}
	name, err := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
	if err != nil {
		return "", false, err
	}
	sec, err := svc.Secrets().Get(ctx, mid, name)
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

// requireSecret is secret plus the fail-closed contract: a declared account
// with a missing/unreadable secret errors the charge — never boot fallback.
func (b *MerchantCollectionAdapterBuilder) requireSecret(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope, key string) (string, error) {
	value, found, err := b.secret(ctx, svc, mid, scope, key)
	name, _ := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, key)
	if err != nil {
		return "", fmt.Errorf("merchant %s rail %s: secret %s backend failed: %w", mid.String(), scope.Rail, name, err)
	}
	if !found {
		return "", fmt.Errorf("merchant %s rail %s: secret %s missing (#725: a declared account never falls back to boot rails)", mid.String(), scope.Rail, name)
	}
	return value, nil
}
