package embed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/config"
	boot "github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

type BillingConfig = boot.BillingConfig
type MerchantConfig = boot.MerchantConfig

// InvoiceConfig aliases the merchant invoice/collection policy block so
// embedded hosts can set it programmatically (#798).
type InvoiceConfig = boot.InvoiceConfig
type MerchantProfileConfig = boot.MerchantProfileConfig
type PSPConfig = boot.PSPConfig

// CheckoutRoutingRuleConfig / CheckoutRoutingMatchConfig alias the or#288
// processor-routing policy block so embedded hosts can declare it
// programmatically alongside their PSPs.
type CheckoutRoutingRuleConfig = boot.CheckoutRoutingRuleConfig
type CheckoutRoutingMatchConfig = boot.CheckoutRoutingMatchConfig

type ProviderRailAccountConfig = boot.ProviderRailAccountConfig
type RailMerchantAccountSignerConfig = boot.RailMerchantAccountSignerConfig
type RemoteApplicationConfig = boot.RemoteApplicationConfig
type StaticJWKSConfig = boot.StaticJWKSConfig
type StaticJWKConfig = boot.StaticJWKConfig

// UpsertMerchantConfig idempotently creates or updates a billing merchant and its
// provider accounts from the embedded engine. Run it as many times as you like —
// create-if-missing, reconcile-if-present — so an embedder (e.g. a legacy-data
// migrate) can just say "here are the payment providers for this merchant" on
// every run. Billing-only: it touches no AuthKit/control-plane state (the merchant
// is an ownerless billing bucket). Already-present secrets are left untouched. If
// the engine is not yet bound to a merchant, it binds to this one; a later call
// with a DIFFERENT slug errors (#770 — one embedded engine serves one merchant).
// Returns the merchant id.
func (rt *Runtime) UpsertMerchantConfig(ctx context.Context, slug string, m MerchantConfig) (merchant.ID, error) {
	if rt == nil || rt.emb == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: runtime not initialized")
	}
	a := rt.emb.App()
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: app database not initialized")
	}
	conf := rt.emb.Config()
	if conf == nil || conf.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: config/db is required")
	}
	database := a.Runtime.DB

	// #770: one embedded engine serves ONE merchant (first upsert binds it; the
	// SDK transport always serves the bound merchant). A second upsert with a
	// different slug used to return nil and leave the client silently pinned to
	// the first merchant — refuse loudly here, BEFORE any rows are written.
	// Re-upserting the bound merchant's own slug stays the normal boot reconcile.
	if bound := a.Runtime.ConfiguredMerchant(); !bound.IsZero() {
		row, err := gen.New(database.DataPool()).GetMerchantBySlug(ctx, merchant.NormalizeSlug(slug))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return merchant.ID{}, fmt.Errorf("openrails embed: engine already bound to merchant %s; one embedded engine serves one merchant — refusing to provision %q (run one engine per merchant)", bound, slug)
		case err != nil:
			return merchant.ID{}, fmt.Errorf("openrails embed: resolve merchant slug %q: %w", slug, err)
		case merchant.ID(row.ID) != bound:
			return merchant.ID{}, fmt.Errorf("openrails embed: engine already bound to merchant %s; one embedded engine serves one merchant — refusing second merchant %q (%s)", bound, slug, merchant.ID(row.ID))
		}
	}

	// Run it through the same billing-only merchant-provisioning boundary embed.New
	// and the bootstrap CLI use, with ControlPlane nil. Provider-account reconcile
	// needs a merchant secret store (built over the engine's own pool).
	req := boot.ProvisionMerchantRequest{
		Config:   conf,
		Database: database,
		Slug:     slug,
		Merchant: m,
		Options:  boot.MerchantManifestReconcileOptions{Insert: true},
	}
	var secretBackend *merchantsecrets.Store
	switch {
	case conf.IsManifestMerchantSource():
		// MODE 1 (#723): this call IS the manifest. Identity/config/account rows
		// reconcile as DB projections; secrets seed the runtime's in-memory plane
		// (never a persistent store). An empty MerchantConfig stays a legal
		// read-side bind (hentai0). ProvisionMerchant forces Insert+Overwrite+
		// Prune so a re-run (host reboot) steamrolls stale in-memory values.
		if a.Runtime == nil || a.Runtime.ManifestSecrets == nil {
			return merchant.ID{}, fmt.Errorf("openrails embed: merchant_source=manifest requires the runtime manifest secret plane (#723)")
		}
		req.SecretStore = a.Runtime.ManifestSecrets.Seeder()
		backend, err := merchantsecrets.BuildManifest(ctx, conf, a.Runtime.ManifestSecrets)
		if err != nil {
			return merchant.ID{}, fmt.Errorf("openrails embed: %w", err)
		}
		secretBackend = backend
		req.SolanaTransit = backend.SolanaTransit
		// Write routes stay mounted so mode-1 mutation calls receive the pointed
		// 405 rejection (not a bare 404); Solana signing keys can live in memory.
		a.Runtime.RouteCapabilities = &routesurface.RuntimeCapabilities{
			SolanaCanSign: backend.SolanaCanSign,
			SecretWrite:   backend.SecretWrite,
		}
	case merchantConfigDeclaresManifestTruth(m):
		// MODE 2 (#724): merchant truth arrives via the HTTP APIs; a manifest-shaped
		// upsert is a second truth and refuses loudly. A bare bind (empty config or
		// display name only) stays legal — it declares no truth.
		return merchant.ID{}, fmt.Errorf("openrails embed: merchant_source=api refuses manifest-declared merchant config (two truths, #723/#724); provision via PUT /v1/merchant/payment-providers and the catalog APIs, or run merchant_source=manifest")
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant().IsZero() {
		a.Runtime.SetConfiguredMerchant(tn.ID)
	}
	// MODE 1: arm the runtime consumers (checkout/vault/webhooks/pulls) over the
	// freshly seeded in-memory plane right away — no standalone server or worker
	// registration ever needs to run first.
	if conf.IsManifestMerchantSource() && a.Runtime.Merchants == nil {
		svc, err := merchants.NewService(database.DataPool(), a.Runtime.ManifestSecrets, config.ExpectedProviderEnvironment(conf.IsTestMode()))
		if err != nil {
			return merchant.ID{}, fmt.Errorf("openrails embed: build merchants service: %w", err)
		}
		a.Runtime.ArmMerchantsService(svc, a.Runtime.ManifestSecrets)
	}
	if conf.IsManifestMerchantSource() {
		a.Runtime.ArmSolanaRecurringServices(secretBackend.Secrets, secretBackend.SolanaTransit)
	}
	return tn.ID, nil
}

// merchantConfigDeclaresManifestTruth reports whether an UpsertMerchantConfig
// payload carries manifest-owned truth (accounts/secrets, profile, invoice
// policy, spend windows, remote-application trust) as opposed to a bare
// identity bind.
func merchantConfigDeclaresManifestTruth(m MerchantConfig) bool {
	return len(m.PSPs) > 0 ||
		m.RemoteApplication != nil ||
		m.Invoice != nil ||
		len(m.DelegatedInvokerWastedSpendWindows) > 0 ||
		len(m.CheckoutRouting) > 0 ||
		strings.TrimSpace(m.Profile.DisplayName) != "" ||
		strings.TrimSpace(m.Profile.LogoURL) != "" ||
		strings.TrimSpace(m.Profile.FromEmail) != "" ||
		strings.TrimSpace(m.Profile.SupportURL) != "" ||
		strings.TrimSpace(m.Profile.SignupURL) != ""
}

// ParseMerchantConfig parses a single merchant YAML document into a MerchantConfig.
// Strict (#711): unknown fields are rejected — the same DisallowUnknownField
// posture as the manifest paths — so a typo'd `acounts:` fails loudly instead
// of silently provisioning a merchant with no rails.
func ParseMerchantConfig(raw []byte) (MerchantConfig, error) {
	// Renamed keys get a pointer instead of a bare "unknown field" (#698/#683).
	var probe map[string]any
	if yaml.Unmarshal(raw, &probe) == nil {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe[old]; ok {
				return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %s was renamed to psps", old)
			}
		}
	}
	var m MerchantConfig
	if err := yaml.UnmarshalWithOptions(raw, &m, yaml.DisallowUnknownField()); err != nil {
		return MerchantConfig{}, fmt.Errorf("openrails embed: parse merchant config: %w", err)
	}
	return m, nil
}

// ParseMerchantConfigManifest parses a multi-merchant config manifest.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return boot.ParseMerchantConfigManifest(raw)
}

// LoadMerchantConfigManifest parses a multi-merchant config manifest and applies
// BILLING_ environment overlays using OpenRails' merchant config mapper.
func LoadMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	return boot.LoadMerchantConfigManifestBytes(raw)
}
