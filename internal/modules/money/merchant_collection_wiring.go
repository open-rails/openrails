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
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
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

// CollectionVerifyResult reports one provider read for an in-doubt charge.
type CollectionVerifyResult struct {
	// Supported = the rail has a provider read for this question.
	Supported bool
	// Settled = a successful charge for the operation exists at the provider.
	Settled           bool
	TransactionID     string
	ExternalInvoiceID string
}

// CollectionReceiptExpectation is the frozen operation every receipt must
// match exactly: its provider identity (the operation key) and the amount
// and currency frozen at enqueue. The instrument is the payment method the
// reads are made for.
type CollectionReceiptExpectation struct {
	OperationKey string
	Amount       moneyutil.Cents
	Currency     string
}

// CollectionVerifier answers the reconciliation reads for one collection
// operation. Implemented by the store-armed credential plane; faked in tests.
type CollectionVerifier interface {
	// VerifyCollectionCharge looks for this operation's settled charge at the
	// provider (NMI-family order reference search) and reads it back through
	// the same exact match operator resolution uses. Supported=false for rails
	// without such a read; an empty search is inconclusive (Settled=false, nil
	// error), never non-execution; a charge that exists but contradicts the
	// frozen facts is an error, never a receipt.
	VerifyCollectionCharge(ctx context.Context, method gen.OpenrailsPaymentMethod, expect CollectionReceiptExpectation) (CollectionVerifyResult, error)
	// ConfirmCollectionReceipt reads the exact provider object an operator
	// named and requires it to be this operation's settled charge.
	ConfirmCollectionReceipt(ctx context.Context, method gen.OpenrailsPaymentMethod, providerReference string, expect CollectionReceiptExpectation) (CollectionVerifyResult, error)
	// ConfirmCollectionNotExecuted errors while the provider shows the
	// operation executed, or when it cannot say.
	ConfirmCollectionNotExecuted(ctx context.Context, method gen.OpenrailsPaymentMethod, expect CollectionReceiptExpectation) error
}

// NMIClientResolver is the raw-client NMI leg of the #725 store resolver.
type NMIClientResolver = railresolve.NMIClientResolver

// MerchantCollectionAdapterBuilder builds one merchant's collection adapter at
// charge time. MerchantsFn is late-bound (Runtime.Merchants is wired after the
// charger is constructed); a nil fn/service = nothing armable.
type MerchantCollectionAdapterBuilder struct {
	Config      *config.Config
	DB          *db.DB
	MerchantsFn func() *merchants.Service
	Endpoints   CollectionEndpoints
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

// VerifyCollectionCharge is the invoice_collection verifier's provider READ:
// for NMI-family rails it arms the store-scoped client and runs the one
// exact-receipt path (nmiCollectionReceipt) with no operator-named
// transaction: the Query API's sale for the operation's order reference is
// the only candidate, and it must read back exactly. Custodian-proxied
// charges settle on the SAME NMI gateway account (or#879), so one branch
// covers both transports. Stripe has no such read and converges through
// idempotent replay instead.
func (b *MerchantCollectionAdapterBuilder) VerifyCollectionCharge(ctx context.Context, method gen.OpenrailsPaymentMethod, expect CollectionReceiptExpectation) (CollectionVerifyResult, error) {
	client, ok, err := b.nmiClientFor(ctx, method)
	if err != nil || !ok {
		return CollectionVerifyResult{}, err
	}
	return nmiCollectionReceipt(ctx, client, method, "", expect)
}

// ConfirmCollectionReceipt reads the exact provider object an operator named.
// NMI: the same exact-receipt path as autonomous verification, with the
// named transaction required to be the order reference's sale. Stripe: the
// invoice must match the operation through the same check the collection
// sequence applies to its own paid invoice.
func (b *MerchantCollectionAdapterBuilder) ConfirmCollectionReceipt(ctx context.Context, method gen.OpenrailsPaymentMethod, providerReference string, expect CollectionReceiptExpectation) (CollectionVerifyResult, error) {
	providerReference = strings.TrimSpace(providerReference)
	if providerReference == "" {
		return CollectionVerifyResult{}, errors.New("provider reference is required")
	}
	if normalizeRail(method.Rail) == string(models.RailStripe) {
		service, err := b.stripeServiceFor(ctx, method)
		if err != nil {
			return CollectionVerifyResult{}, err
		}
		receipt, found, err := service.GetCollectionInvoice(ctx, providerReference)
		if err != nil {
			return CollectionVerifyResult{}, err
		}
		if !found {
			return CollectionVerifyResult{}, fmt.Errorf("stripe invoice %s does not exist", providerReference)
		}
		if err := receipt.MatchesOperation(expect.OperationKey, expect.Amount, expect.Currency); err != nil {
			return CollectionVerifyResult{}, err
		}
		return CollectionVerifyResult{Supported: true, Settled: true, TransactionID: stripeReceiptTransactionID(receipt), ExternalInvoiceID: receipt.InvoiceID}, nil
	}
	client, ok, err := b.nmiClientFor(ctx, method)
	if err != nil {
		return CollectionVerifyResult{}, err
	}
	if !ok {
		return CollectionVerifyResult{}, fmt.Errorf("rail %q has no armed provider read", method.Rail)
	}
	res, err := nmiCollectionReceipt(ctx, client, method, providerReference, expect)
	if err != nil {
		return CollectionVerifyResult{}, err
	}
	if !res.Settled {
		return CollectionVerifyResult{}, fmt.Errorf("transaction %s is not the successful sale for order %s", providerReference, expect.OperationKey)
	}
	return res, nil
}

// nmiCollectionReceipt is the ONE exact-receipt path for an NMI-family
// collection, shared by autonomous verification and operator resolution. The
// Query API must return a successful sale for the operation's order reference
// (identity); with providerReference set it must be that very sale. The v5
// read of that sale must then be approved, in the frozen currency, for the
// frozen amount, on the instrument's customer vault. A custodian-held card
// (or#879) is charged by card data and has no vault at NMI: its exact read
// binds approval, currency and amount, and the order reference binds the
// instrument. An empty search is inconclusive (Settled=false, nil error); a
// sale that exists but contradicts the frozen facts is an error.
func nmiCollectionReceipt(ctx context.Context, client *nmi.NMIClient, method gen.OpenrailsPaymentMethod, providerReference string, expect CollectionReceiptExpectation) (CollectionVerifyResult, error) {
	key := strings.TrimSpace(expect.OperationKey)
	if key == "" || expect.Amount <= 0 || strings.TrimSpace(expect.Currency) == "" {
		return CollectionVerifyResult{}, errors.New("collection receipt expectation is incomplete")
	}
	txnID, found, err := client.FindSuccessfulSaleByOrderID(ctx, key)
	if err != nil {
		return CollectionVerifyResult{}, fmt.Errorf("nmi query for order ref %q: %w", key, err)
	}
	txnID = strings.TrimSpace(txnID)
	if !found || txnID == "" {
		if providerReference != "" {
			return CollectionVerifyResult{}, fmt.Errorf("transaction %s is not the successful sale for order %s", providerReference, key)
		}
		return CollectionVerifyResult{Supported: true}, nil
	}
	if providerReference != "" && txnID != providerReference {
		return CollectionVerifyResult{}, fmt.Errorf("transaction %s is not the successful sale for order %s", providerReference, key)
	}
	if method.Custodian == models.CustodianBasisTheory {
		err = client.ConfirmApprovedUnvaultedSale(ctx, txnID, expect.Amount, expect.Currency)
	} else {
		err = client.ConfirmApprovedSale(ctx, txnID, strings.TrimSpace(method.RailCustomerRef), expect.Amount, expect.Currency)
	}
	if err != nil {
		return CollectionVerifyResult{}, err
	}
	return CollectionVerifyResult{Supported: true, Settled: true, TransactionID: txnID}, nil
}

// ConfirmCollectionNotExecuted refuses provider-confirmed non-execution while
// the provider shows a successful charge for the operation, or when it
// cannot say. For Stripe it also makes the non-execution definitive: every
// invoice item, draft and open invoice stamped with the operation key is
// deleted or voided so no later invoice can sweep them; a paid one refuses.
func (b *MerchantCollectionAdapterBuilder) ConfirmCollectionNotExecuted(ctx context.Context, method gen.OpenrailsPaymentMethod, expect CollectionReceiptExpectation) error {
	if normalizeRail(method.Rail) == string(models.RailStripe) {
		service, err := b.stripeServiceFor(ctx, method)
		if err != nil {
			return err
		}
		customerID, err := NewStripeCollectionAdapter(b.DB, service).stripeCustomerID(ctx, method)
		if err != nil {
			return err
		}
		return service.CleanupCollection(ctx, customerID, expect.OperationKey)
	}
	res, err := b.VerifyCollectionCharge(ctx, method, expect)
	if errors.Is(err, nmi.ErrReceiptMismatch) {
		return fmt.Errorf("provider shows a sale for order %s that contradicts the operation: %w", expect.OperationKey, err)
	}
	if err != nil {
		return err
	}
	if !res.Supported {
		return fmt.Errorf("rail %q has no armed provider read", method.Rail)
	}
	if res.Settled {
		return fmt.Errorf("provider shows successful sale %s for order %s", res.TransactionID, expect.OperationKey)
	}
	return nil
}

// nmiClientFor arms the NMI client for an NMI-family instrument's stamped
// account. ok=false for other rails or an undeclared account.
func (b *MerchantCollectionAdapterBuilder) nmiClientFor(ctx context.Context, method gen.OpenrailsPaymentMethod) (*nmi.NMIClient, bool, error) {
	if b == nil || b.merchants() == nil || b.DB == nil || !rails.IsNMI(models.Rail(normalizeRail(method.Rail))) {
		return nil, false, nil
	}
	return b.ResolveNMIClient(ctx, method.MerchantID, &method.PspID)
}

func (b *MerchantCollectionAdapterBuilder) stripeServiceFor(ctx context.Context, method gen.OpenrailsPaymentMethod) (*subscriptions.StripeService, error) {
	svc := b.merchants()
	if b == nil || svc == nil || b.DB == nil {
		return nil, errors.New("stripe collection plane is not armed")
	}
	mid := merchant.ID(method.MerchantID)
	scope, ok, err := b.resolveScope(ctx, svc, mid, string(models.RailStripe), &method.PspID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("merchant %s declares no stripe account", mid.String())
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
