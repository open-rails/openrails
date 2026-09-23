package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #725/#788: the invoice collection plane arms rail credentials PER
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
// surface (#725/#788): store-armed collection adapters, raw NMI clients and
// the reconciliation reads for in-doubt collection operations.
// Satisfied by *MerchantCollectionAdapterBuilder; tests may inject fakes.
type CollectionPlane interface {
	CollectionAdapterResolver
	NMIClientResolver
	CollectionVerifier
}

// CollectionVerifier reads qualified receipts for a canonical accepted row.
// Nonexecution after possible submission requires provider-supported proof;
// absence from a search alone never releases a potentially charged operation.
type CollectionVerifier interface {
	ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error)
	ConfirmCollectionNotExecuted(ctx context.Context, in gen.OpenrailsRailIntent) error
}

// NMIClientResolver is the raw-client NMI leg of the #725 store resolver.
type NMIClientResolver = railresolve.NMIClientResolver

// MerchantCollectionAdapterBuilder builds one merchant's collection adapter at
// charge time. MerchantsFn is late-bound (Runtime.Merchants is wired after the
// charger is constructed); a nil fn/service = nothing armable.
type MerchantCollectionAdapterBuilder struct {
	StripeClients *stripeapi.Factory
	Config        *config.Config
	DB            *db.DB
	MerchantsFn   func() *merchants.Service
	Endpoints     CollectionEndpoints
}

var _ CollectionPlane = (*MerchantCollectionAdapterBuilder)(nil)

func (b *MerchantCollectionAdapterBuilder) merchants() *merchants.Service {
	if b == nil || b.MerchantsFn == nil {
		return nil
	}
	return b.MerchantsFn()
}

func (b *MerchantCollectionAdapterBuilder) testMode() bool {
	return b.Config != nil && b.Config.IsTestMode()
}

// nmiArmer is the shared store-armed NMI client plane this builder delegates
// scope, secret and client construction to.
func (b *MerchantCollectionAdapterBuilder) nmiArmer() *railresolve.NMIArmer {
	if b == nil {
		return nil
	}
	return &railresolve.NMIArmer{Config: b.Config, DB: b.DB, MerchantsFn: b.MerchantsFn, Endpoints: railresolve.NMIEndpoints{
		V5BaseURL: b.Endpoints.NMIV5BaseURL, DirectPostURL: b.Endpoints.NMIDirectPostURL, QueryURL: b.Endpoints.NMIQueryURL,
	}}
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
	scope, ok, err := b.resolveScope(ctx, svc, mid, rail, &method.PspID)
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
		scope.CustodianID = method.CustodianID
		adapter, err = b.custodianProxyAdapter(ctx, svc, mid, scope)
	case method.Custodian == models.CustodianHyperSwitch:
		scope.CustodianID = method.CustodianID
		adapter, err = b.hyperSwitchAdapter(ctx, svc, mid, scope)
	case method.Custodian != models.CustodianPSP:
		return nil, false, fmt.Errorf("custodian %s has no qualified invoice collection transport", method.Custodian)
	default:
		adapter, err = b.nmiAdapter(ctx, svc, mid, scope)
	}
	if err != nil {
		return nil, false, err
	}
	return adapter, true, nil
}

// resolveScope picks the account the charge settles through: the stamped
// provenance account when present (archived stays addressable for existing
// obligations), else the pull scope.
func (b *MerchantCollectionAdapterBuilder) resolveScope(ctx context.Context, _ *merchants.Service, mid merchant.ID, rail string, stamped *uuid.UUID) (merchants.PSPScope, bool, error) {
	return b.nmiArmer().ResolveScope(ctx, mid, rail, stamped)
}

// ResolveNMIClient arms the store-scoped NMI client for one write: the
// stamped provenance account when present, else the NMI pull scope.
func (b *MerchantCollectionAdapterBuilder) ResolveNMIClient(ctx context.Context, merchantID uuid.UUID, stampedAccountID *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if b == nil {
		return nil, false, nil
	}
	return b.nmiArmer().ResolveNMIClient(ctx, merchantID, stampedAccountID)
}

// ConfirmCollectionNotExecuted closes only a provider-provable nonexecution.
// NMI search absence is not authoritative. Stripe objects can be cleaned up
// under the same operation key, then read back in an unchargeable state.
func (b *MerchantCollectionAdapterBuilder) ConfirmCollectionNotExecuted(ctx context.Context, in gen.OpenrailsRailIntent) error {
	p, err := intents.DecodeInvoiceCollectionPayload(in)
	if err != nil {
		return err
	}
	if p.Rail == "stripe" {
		service, err := b.stripeServiceFor(ctx, in)
		if err != nil {
			return err
		}
		return service.CleanupCollection(ctx, p.ProviderCustomerRef, in.ID.String())
	}
	receipt, found, err := b.ReadCollectionReceipt(ctx, in, "")
	if errors.Is(err, nmi.ErrReceiptMismatch) {
		return fmt.Errorf("provider evidence contradicts the operation: %w", err)
	}
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("provider shows successful sale %s; nonexecution is contradicted", receipt.TransactionID())
	}
	return errors.New("NMI search absence cannot prove nonexecution after possible submission")
}

func (b *MerchantCollectionAdapterBuilder) stripeServiceFor(ctx context.Context, in gen.OpenrailsRailIntent) (*subscriptions.StripeService, error) {
	if _, err := intents.DecodeInvoiceCollectionPayload(in); err != nil {
		return nil, err
	}
	svc := b.merchants()
	if svc == nil || b.DB == nil {
		return nil, errors.New("Stripe collection plane is not armed")
	}
	mid := merchant.ID(in.MerchantID)
	scope, ok, err := b.resolveScope(ctx, svc, mid, "stripe", in.PspID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("accepted Stripe account cannot be armed")
	}
	return b.stripeService(ctx, svc, mid, scope)
}

func (b *MerchantCollectionAdapterBuilder) stripeAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	service, err := b.stripeService(ctx, svc, mid, scope)
	if err != nil {
		return nil, err
	}
	return NewStripeCollectionAdapter(b.DB, service), nil
}

func (b *MerchantCollectionAdapterBuilder) stripeService(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (*subscriptions.StripeService, error) {
	secretKey, err := b.requireSecret(ctx, svc, mid, scope, "secret_key")
	if err != nil {
		return nil, err
	}
	service := subscriptions.NewAccountStripeService(b.Config, mid.UUID(), scope.ID, scope.AccountID, secretKey)
	service.StripeClients = b.StripeClients
	if b.Endpoints.StripeBaseURL != "" {
		service.SetBaseURLForTest(b.Endpoints.StripeBaseURL)
	}
	return service, nil
}

// custodianProxyAdapter arms the #795 detokenizing-proxy collection adapter:
// the custodian's private app key and THIS PSP's own gateway security key. The
// gateway half is the PSP's (or#879 folded the old cross-account pointer away);
// the custodial half is the referenced custodian's (or#880), so several PSPs
// charging the same vault share ONE credential rather than a copy each.
func (b *MerchantCollectionAdapterBuilder) custodianProxyAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	if scope.CustodianID == nil {
		return nil, fmt.Errorf("psp %s/%s: instrument is held by custodian %s but the PSP references none", scope.Rail, scope.AccountID, models.CustodianBasisTheory)
	}
	custodian, ok, err := svc.CustodianScopeByID(ctx, mid, *scope.CustodianID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("psp %s/%s: %w (%s)", scope.Rail, scope.AccountID, merchants.ErrCustodianNotDeclared, *scope.CustodianID)
	}
	apiKey, err := b.requireCustodianSecret(ctx, svc, mid, custodian, custodians.SecretAPIKey)
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
	destination := gw.DirectPostURL
	if destination == "" {
		destination = nmi.DefaultDirectPostURL
	}
	if gw.Posture, err = nmi.ProxyPostureClient(gatewayKey, destination, b.testMode(), gw.DirectPostURL != ""); err != nil {
		return nil, err
	}
	return NewCustodianProxyCollectionAdapter(nmiproxy.New(bt, gw)), nil
}

func (b *MerchantCollectionAdapterBuilder) nmiAdapter(ctx context.Context, svc *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (CollectionAdapter, error) {
	client, err := b.nmiClient(ctx, svc, mid, scope)
	if err != nil {
		return nil, err
	}
	return NewNMICollectionAdapter(client), nil
}

// nmiClient builds the store-armed NMI client for scope.
func (b *MerchantCollectionAdapterBuilder) nmiClient(ctx context.Context, _ *merchants.Service, mid merchant.ID, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	return b.nmiArmer().NMIClient(ctx, mid, scope)
}

func (b *MerchantCollectionAdapterBuilder) requireSecret(ctx context.Context, _ *merchants.Service, mid merchant.ID, scope merchants.PSPScope, key string) (string, error) {
	return b.nmiArmer().RequireSecret(ctx, mid, scope, key)
}

// requireCustodianSecret is the custody sibling: the credential is scoped to
// the CUSTODIAN's identity, not to the PSP that happens to charge through it.
func (b *MerchantCollectionAdapterBuilder) requireCustodianSecret(ctx context.Context, svc *merchants.Service, mid merchant.ID, custodian merchants.CustodianScope, key string) (string, error) {
	// or#812: same versioned read as every other provider credential.
	ref, err := custodian.SecretRef(key)
	if err != nil {
		return "", err
	}
	if svc == nil || svc.Secrets() == nil {
		return "", fmt.Errorf("merchant %s custodian %s: secret store is not armed", mid.String(), custodian.Key)
	}
	sec, err := merchants.ReadSecretRef(ctx, svc.Secrets(), mid, ref)
	if errors.Is(err, merchants.ErrSecretNotFound) {
		return "", fmt.Errorf("merchant %s custodian %s: secret %s missing (#725: a declared custodian never falls back to boot rails)", mid.String(), custodian.Key, ref.Name)
	}
	if err != nil {
		return "", fmt.Errorf("merchant %s custodian %s: secret %s backend failed: %w", mid.String(), custodian.Key, ref.Name, err)
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return "", fmt.Errorf("merchant %s custodian %s: secret %s is empty", mid.String(), custodian.Key, ref.Name)
	}
	return value, nil
}

// ReadCollectionReceipt resolves the immutable accepted account before reading
// provider facts. A boolean reconciliation result cannot settle a collection.
func (b *MerchantCollectionAdapterBuilder) ReadCollectionReceipt(ctx context.Context, in gen.OpenrailsRailIntent, reference string) (intents.CollectedReceipt, bool, error) {
	p, err := intents.DecodeInvoiceCollectionPayload(in)
	if err != nil {
		return intents.CollectedReceipt{}, false, err
	}
	if p.Rail != "stripe" {
		return intents.ReadNMICollectionReceipt(ctx, in, b, reference)
	}
	service, err := b.stripeServiceFor(ctx, in)
	if err != nil {
		return intents.CollectedReceipt{}, false, err
	}
	return intents.ReadStripeCollectionReceipt(ctx, in, service, reference)
}
