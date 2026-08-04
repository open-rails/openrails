package bootstrap

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/go-viper/mapstructure/v2"
	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
	jwtkit "github.com/open-rails/authkit/jwtkit"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestAdvisoryLock = int64(734252042137424)

const DefaultMerchantConfigManifestPath = "/etc/openrails/merchants.yaml"

type BillingConfig struct {
	Version   int                       `yaml:"version" koanf:"version"`
	Merchants map[string]MerchantConfig `yaml:"merchants" koanf:"merchants"`
}

// LoadMerchantConfigManifest reads and validates a merchant config manifest.
func LoadMerchantConfigManifest(path string) (*BillingConfig, error) {
	return LoadMerchantConfigManifestFiles(path)
}

// LoadMerchantConfigManifestFiles loads a merchant config YAML file plus optional
// structured YAML overlays through koanf, then validates the merged tree.
func LoadMerchantConfigManifestFiles(path string, overlays ...string) (*BillingConfig, error) {
	k := koanf.New(".")
	for _, p := range append([]string{path}, overlays...) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("read merchant config manifest %s: %w", p, err)
		}
		if err := k.Load(file.Provider(p), koanfyaml.Parser()); err != nil {
			return nil, fmt.Errorf("load merchant config manifest %s: %w", p, err)
		}
	}
	for _, key := range []string{"auth", "users", "groups", "roles", "permissions", "catalogs", "products"} {
		if k.Exists(key) {
			return nil, fmt.Errorf("merchant config manifest does not accept %q; use the matching push command", key)
		}
	}
	// Strict-parse the merged tree: koanf's Unmarshal silently ignores unknown
	// fields, so route the file/overlay path through the same DisallowUnknownField
	// parser as the embedded bytes path — a typo'd field is rejected, not dropped.
	merged, err := k.Marshal(koanfyaml.Parser())
	if err != nil {
		return nil, fmt.Errorf("merge merchant config manifest: %w", err)
	}
	return ParseMerchantConfigManifest(merged)
}

func LoadMerchantConfigManifestBytes(raw []byte) (*BillingConfig, error) {
	manifest, err := ParseMerchantConfigManifest(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectRenamedMerchantEnvVars(); err != nil {
		return nil, err
	}
	k := koanf.New(".")
	// Operator-mounted secret files (filename = env-var name) load BELOW the
	// env overlay, so env wins. Default non-SaaS path: Vault renders
	// BILLING_MERCHANTS_* files into the mounted dir; no live Vault needed.
	fileOverlay, err := merchantSecretFileOverlay()
	if err != nil {
		return nil, err
	}
	if len(fileOverlay) > 0 {
		if err := k.Load(confmap.Provider(fileOverlay, "."), nil); err != nil {
			return nil, fmt.Errorf("load merchant config secret-file overlay: %w", err)
		}
	}
	if err := k.Load(env.ProviderWithValue(MerchantBillingEnvPrefix, ".", merchantBillingEnvKV), nil); err != nil {
		return nil, fmt.Errorf("load merchant config env overlay: %w", err)
	}
	if len(k.Keys()) > 0 {
		var overlay BillingConfig
		// Strict, matching the file path's DisallowUnknownField (#710): a var that
		// routes to a section but names an unknown field errors, never drops.
		if err := k.UnmarshalWithConf("", &overlay, koanf.UnmarshalConf{
			Tag: "koanf",
			DecoderConfig: &mapstructure.DecoderConfig{
				DecodeHook: mapstructure.ComposeDecodeHookFunc(
					mapstructure.StringToTimeDurationHookFunc(),
					mapstructure.StringToSliceHookFunc(","),
					mapstructure.TextUnmarshallerHookFunc(),
				),
				Result:           &overlay,
				WeaklyTypedInput: true,
				ErrorUnused:      true,
			},
		}); err != nil {
			return nil, fmt.Errorf("unmarshal merchant config env overlay: %w", err)
		}
		mergeMerchantConfigManifest(manifest, &overlay)
		if err := validateMerchantManifestShape(manifest); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

// merchantSecretFileOverlay maps operator-mounted secret files
// (config.SecretFiles: filename = env-var name, content = value) through the
// same routing and rejection rules as BILLING_* env vars.
func merchantSecretFileOverlay() (map[string]any, error) {
	files, err := config.SecretFiles()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for name, value := range files {
		if !strings.HasPrefix(name, MerchantBillingEnvPrefix) {
			continue
		}
		if err := rejectRenamedMerchantEnvName("secret file", name); err != nil {
			return nil, err
		}
		key, v := merchantBillingEnvKV(name, value)
		if key == "" {
			continue
		}
		out[key] = v
	}
	return out, nil
}

// merchantBillingEnvKV maps a BILLING_* env var to its koanf key and value.
// JSON array/object values decode structurally so list-valued manifest fields
// (delegated_invoker_wasted_spend_windows) can be overlaid from one var.
func merchantBillingEnvKV(name, value string) (string, any) {
	key := MerchantBillingEnvKey(name)
	if key == "" {
		return "", nil
	}
	v := strings.TrimSpace(value)
	if len(v) >= 2 && ((v[0] == '[' && v[len(v)-1] == ']') || (v[0] == '{' && v[len(v)-1] == '}')) {
		var decoded any
		if err := json.Unmarshal([]byte(v), &decoded); err == nil {
			return key, decoded
		}
	}
	return key, v
}

// ParseMerchantConfigManifest parses the merchant config manifest consumed by
// push-merchant-config. Bootstrap authority and catalog state are intentionally
// rejected by the strict YAML decoder.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	if err := rejectRenamedMerchantConfigKeys(raw); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
	var manifest BillingConfig
	if err := yaml.UnmarshalWithOptions(raw, &manifest, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
	if len(manifest.Merchants) == 0 {
		return nil, fmt.Errorf("merchant config manifest must declare at least one merchant")
	}
	if err := validateMerchantManifestShape(&manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// rejectRenamedMerchantConfigKeys fails a manifest that still spells the
// psps key by a retired name (psps <- accounts <- rail_merchant_accounts <-
// provider_accounts). The strict parser would
// reject these as unknown fields anyway, but "unknown field" reads like a typo
// — a rename deserves a pointer. Silent-ignore is the worst failure here: it
// would apply an empty account set.
func rejectRenamedMerchantConfigKeys(raw []byte) error {
	var probe struct {
		Merchants map[string]map[string]any `yaml:"merchants"`
	}
	if yaml.Unmarshal(raw, &probe) != nil {
		return nil // malformed YAML: let the strict parser report it
	}
	slugs := make([]string, 0, len(probe.Merchants))
	for slug := range probe.Merchants {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		for _, old := range []string{"rail_merchant_accounts", "provider_accounts"} {
			if _, ok := probe.Merchants[slug][old]; ok {
				return fmt.Errorf("merchants.%s.%s was renamed to psps", slug, old)
			}
		}
	}
	return nil
}

func mergeMerchantConfigManifest(dst, src *BillingConfig) {
	if dst == nil || src == nil {
		return
	}
	if src.Version != 0 {
		dst.Version = src.Version
	}
	if len(src.Merchants) == 0 {
		return
	}
	if dst.Merchants == nil {
		dst.Merchants = map[string]MerchantConfig{}
	}
	for slug, srcMerchant := range src.Merchants {
		dstMerchant := dst.Merchants[slug]
		mergeMerchantConfig(&dstMerchant, srcMerchant)
		dst.Merchants[slug] = dstMerchant
	}
}

func mergeMerchantConfig(dst *MerchantConfig, src MerchantConfig) {
	if strings.TrimSpace(src.DisplayName) != "" {
		dst.DisplayName = src.DisplayName
	}
	if strings.TrimSpace(src.APIHost) != "" {
		dst.APIHost = src.APIHost
	}
	if src.RemoteApplication != nil {
		dst.RemoteApplication = src.RemoteApplication
	}
	mergeMerchantProfileConfig(&dst.Profile, src.Profile)
	if src.Invoice != nil {
		if dst.Invoice == nil {
			dst.Invoice = &InvoiceConfig{}
		}
		mergeInvoiceConfig(dst.Invoice, src.Invoice)
	}
	if len(src.DelegatedInvokerWastedSpendWindows) > 0 {
		dst.DelegatedInvokerWastedSpendWindows = src.DelegatedInvokerWastedSpendWindows
	}
	if len(src.CheckoutRouting) > 0 {
		dst.CheckoutRouting = src.CheckoutRouting
	}
	if len(src.PSPs) > 0 {
		if dst.PSPs == nil {
			dst.PSPs = map[string]PSPConfig{}
		}
		for key, srcRails := range src.PSPs {
			dstRails := dst.PSPs[key]
			if dstRails == nil {
				dstRails = PSPConfig{}
			}
			for rail, srcAccount := range srcRails {
				dstAccount := dstRails[rail]
				mergeProviderRailAccountConfig(&dstAccount, srcAccount)
				dstRails[rail] = dstAccount
			}
			dst.PSPs[key] = dstRails
		}
	}
}

func mergeMerchantProfileConfig(dst *MerchantProfileConfig, src MerchantProfileConfig) {
	if strings.TrimSpace(src.DisplayName) != "" {
		dst.DisplayName = src.DisplayName
	}
	if strings.TrimSpace(src.LogoURL) != "" {
		dst.LogoURL = src.LogoURL
	}
	if strings.TrimSpace(src.FromEmail) != "" {
		dst.FromEmail = src.FromEmail
	}
	if strings.TrimSpace(src.SupportURL) != "" {
		dst.SupportURL = src.SupportURL
	}
	if strings.TrimSpace(src.SignupURL) != "" {
		dst.SignupURL = src.SignupURL
	}
}

func mergeInvoiceConfig(dst, src *InvoiceConfig) {
	if src.CollectionThreshold != nil {
		dst.CollectionThreshold = src.CollectionThreshold
	}
	if src.MonthlyFloor != nil {
		dst.MonthlyFloor = src.MonthlyFloor
	}
	if strings.TrimSpace(src.BillingPeriodBoundary) != "" {
		dst.BillingPeriodBoundary = src.BillingPeriodBoundary
	}
}

func mergeProviderRailAccountConfig(dst *ProviderRailAccountConfig, src ProviderRailAccountConfig) {
	if strings.TrimSpace(src.LegacyEnvironment) != "" {
		dst.LegacyEnvironment = src.LegacyEnvironment
	}
	if strings.TrimSpace(src.AccountID) != "" {
		dst.AccountID = src.AccountID
	}
	if src.Archived {
		dst.Archived = true
	}
	if src.Signer != nil {
		dst.Signer = src.Signer
	}
	if len(src.Secrets) > 0 {
		if dst.Secrets == nil {
			dst.Secrets = map[string]string{}
		}
		for key, value := range src.Secrets {
			dst.Secrets[key] = value
		}
	}
	if len(src.Settings) > 0 {
		if dst.Settings == nil {
			dst.Settings = map[string]any{}
		}
		for key, value := range src.Settings {
			dst.Settings[key] = value
		}
	}
}

type MerchantConfig struct {
	DisplayName string `yaml:"display_name" koanf:"display_name"`
	// APIHost is the merchant's canonical #734 API host (e.g. "api.myapp.example"):
	// the Host-header value public routes resolve this merchant from (#850).
	// Globally unique across active merchants. Omitted leaves the stored value
	// untouched (it can also be assigned via PUT /v1/merchant/api-host).
	APIHost string `yaml:"api_host,omitempty" koanf:"api_host"`
	// RemoteApplication is the host application's AuthKit remote_application trust
	// for THIS merchant (#527). When set, it is registered as owner of the
	// merchant's permission-group, so host-app delegated tokens administer this
	// merchant — and only this merchant.
	RemoteApplication *RemoteApplicationConfig `yaml:"remote_application,omitempty" koanf:"remote_application"`
	Profile           MerchantProfileConfig    `yaml:"profile,omitempty" koanf:"profile"`
	// Invoice is the merchant's billing/collection policy (#643/#646): when/how the
	// accrued balance is invoiced. Omitted leaves all values at the service default;
	// an omitted field within the block leaves that field as-is.
	Invoice *InvoiceConfig `yaml:"invoice,omitempty" koanf:"invoice"`
	// DelegatedInvokerWastedSpendWindows are merchant-wide abuse cutoffs for
	// delegated invokers (#646): per-window spend ceilings on wasted (failed/abused)
	// generation. Empty leaves the service-default windows (burst 15m/$5, sustained 5h/$20).
	DelegatedInvokerWastedSpendWindows []BudgetWindowConfig `yaml:"delegated_invoker_wasted_spend_windows,omitempty" koanf:"delegated_invoker_wasted_spend_windows"`
	// PSPs is the operator-declared PSP catalog: the merchant's payment
	// service providers, keyed by PSP key (e.g. mobius), one rail each.
	// merchants.<slug>. nothing else it could mean). ONE word everywhere: the
	// DB table (openrails.psps) and the merchant-secret name prefix (`psps/…`)
	// speak the same vocabulary as this key.
	PSPs map[string]PSPConfig `yaml:"psps,omitempty" koanf:"psps"`
	// CheckoutRouting (or#288) is the merchant's processor preference policy:
	// ordered rules, first match wins, each naming a ranked PSP list. Omitted
	// leaves the stored policy untouched; declared REPLACES it whole (an
	// ordered list has no meaningful per-element merge).
	CheckoutRouting []CheckoutRoutingRuleConfig `yaml:"checkout_routing,omitempty" koanf:"checkout_routing"`
}

// CheckoutRoutingRuleConfig is one manifest routing rule. Prefer speaks the
// checkout selector vocabulary (#848): PSP keys, or a rail kind where exactly
// one PSP is armed on it.
type CheckoutRoutingRuleConfig struct {
	Match  CheckoutRoutingMatchConfig `yaml:"match,omitempty" koanf:"match"`
	Prefer []string                   `yaml:"prefer" koanf:"prefer"`
}

// CheckoutRoutingMatchConfig is a rule's condition; every set field must match,
// and an all-empty match is the catch-all (which must be the last rule).
type CheckoutRoutingMatchConfig struct {
	Currency string `yaml:"currency,omitempty" koanf:"currency"`
	Product  string `yaml:"product,omitempty" koanf:"product"`
	Price    string `yaml:"price,omitempty" koanf:"price"`
	Mode     string `yaml:"mode,omitempty" koanf:"mode"`
	Country  string `yaml:"country,omitempty" koanf:"country"`
}

// checkoutRoutingRules projects the manifest rules onto the stored model.
func checkoutRoutingRules(in []CheckoutRoutingRuleConfig) []models.CheckoutRoutingRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]models.CheckoutRoutingRule, 0, len(in))
	for _, rule := range in {
		out = append(out, models.CheckoutRoutingRule{
			Match: models.CheckoutRoutingMatch{
				Currency: rule.Match.Currency,
				Product:  rule.Match.Product,
				Price:    rule.Match.Price,
				Mode:     rule.Match.Mode,
				Country:  rule.Match.Country,
			},
			Prefer: rule.Prefer,
		})
	}
	return out
}

// InvoiceConfig is the merchant invoice/collection policy block, mirroring
// the merchant_configurations invoice fields. Amounts are in the currency's micros.
type InvoiceConfig struct {
	// CollectionThreshold: invoice an arrears customer once their accrued balance
	// reaches this (micros). Default 50_000_000 ($50).
	CollectionThreshold *int64 `yaml:"collection_threshold,omitempty" koanf:"collection_threshold"`
	// MonthlyFloor: don't bother collecting below this (micros). Default 1_000_000 ($1).
	MonthlyFloor *int64 `yaml:"monthly_floor,omitempty" koanf:"monthly_floor"`
	// BillingPeriodBoundary: calendar_month | anniversary | fixed_interval.
	// Default fixed_interval (rolling 30d). calendar_month resets on the 1st.
	BillingPeriodBoundary string `yaml:"billing_period_boundary,omitempty" koanf:"billing_period_boundary"`
	// DelinquencyGraceDays (or#878): days past an invoice's due date before the
	// payer is DELINQUENT — new spend refused and the host signalled to shut off
	// whatever it runs. Business policy, so it is the merchant's. Default 14;
	// 0 means delinquent as soon as it is overdue.
	DelinquencyGraceDays *int `yaml:"delinquency_grace_days,omitempty" koanf:"delinquency_grace_days"`
	// DelinquencyAmountFloor (micros): the smallest overdue balance that can
	// escalate. Unset DERIVES from monthly_floor — a debt too small to bother
	// collecting is too small to cut anyone off for.
	DelinquencyAmountFloor *int64 `yaml:"delinquency_amount_floor,omitempty" koanf:"delinquency_amount_floor"`
}

// BudgetWindowConfig is one delegated-invoker wasted-spend window. Window is a
// Go duration ("15m", "5h"); Limit is the per-window ceiling in the currency's micros.
type BudgetWindowConfig struct {
	Key      string `yaml:"key" koanf:"key"`
	Window   string `yaml:"window" koanf:"window"`
	Limit    int64  `yaml:"limit" koanf:"limit"`
	Currency string `yaml:"currency,omitempty" koanf:"currency"`
}

// RemoteApplicationConfig declares the host-app remote_application trusted for a
// merchant. Provide exactly one trust source: jwks_uri (auto-rotating), jwks
// (static JSON Web Key Set), or public_keys (static PEMs).
type RemoteApplicationConfig struct {
	// Issuer is the token `iss` value.
	Issuer string `yaml:"issuer" koanf:"issuer"`
	// JWKSURI is the remote JWKS endpoint. Mutually exclusive with JWKS/PublicKeys.
	JWKSURI string `yaml:"jwks_uri,omitempty" koanf:"jwks_uri"`
	// JWKS is a static JWKS document. Mutually exclusive with JWKSURI/PublicKeys.
	JWKS StaticJWKSConfig `yaml:"jwks,omitempty" koanf:"jwks"`
	// PublicKeys are static verification keys (PEM). Mutually exclusive with JWKSURI/JWKS.
	PublicKeys []authkit.RemoteAppKey `yaml:"public_keys,omitempty" koanf:"public_keys"`
	// Slug overrides the remote_application slug (defaults to "<merchant>-app").
	Slug string `yaml:"slug,omitempty" koanf:"slug"`
}

type StaticJWKSConfig struct {
	Keys []StaticJWKConfig `yaml:"keys,omitempty" koanf:"keys"`
}

type StaticJWKConfig struct {
	Kty string `yaml:"kty" koanf:"kty"`
	Use string `yaml:"use,omitempty" koanf:"use"`
	Kid string `yaml:"kid,omitempty" koanf:"kid"`
	Alg string `yaml:"alg,omitempty" koanf:"alg"`
	N   string `yaml:"n,omitempty" koanf:"n"`
	E   string `yaml:"e,omitempty" koanf:"e"`
	Crv string `yaml:"crv,omitempty" koanf:"crv"`
	X   string `yaml:"x,omitempty" koanf:"x"`
	Y   string `yaml:"y,omitempty" koanf:"y"`
}

func (j StaticJWKConfig) authkitJWK() jwtkit.JWK {
	return jwtkit.JWK{
		Kty: j.Kty, Use: j.Use, Kid: j.Kid, Alg: j.Alg,
		N: j.N, E: j.E, Crv: j.Crv, X: j.X, Y: j.Y,
	}
}

type MerchantProfileConfig struct {
	DisplayName string `yaml:"display_name,omitempty" koanf:"display_name"`
	LogoURL     string `yaml:"logo_url,omitempty" koanf:"logo_url"`
	FromEmail   string `yaml:"from_email,omitempty" koanf:"from_email"`
	SupportURL  string `yaml:"support_url,omitempty" koanf:"support_url"`
	SignupURL   string `yaml:"signup_url,omitempty" koanf:"signup_url"`
}

type PSPConfig map[string]ProviderRailAccountConfig

type ProviderRailAccountConfig struct {
	// LegacyEnvironment keeps the retired `environment:` key parseable ONLY so a
	// manifest that still declares it fails loudly (#882). The environment is
	// DERIVED from test_mode — it never was anything else, since a declared value
	// that disagreed refused to boot and one that agreed was a no-op.
	LegacyEnvironment string                           `yaml:"environment,omitempty" koanf:"environment"`
	AccountID         string                           `yaml:"account_id,omitempty" koanf:"account_id"`
	Archived          bool                             `yaml:"archived,omitempty" koanf:"archived"`
	Signer            *RailMerchantAccountSignerConfig `yaml:"signer,omitempty" koanf:"signer"`
	Secrets           map[string]string                `yaml:"secrets,omitempty" koanf:"secrets"`
	// Settings are per-account NON-SECRET runtime knobs, stored on the
	// psps row (NMI: tokenization_key/tokenization_url;
	// Solana: rpc_provider, rpc_api_key, tokens,
	// recipient_wallet — see config.SolanaAccountSettings, #711).
	Settings map[string]any `yaml:"settings,omitempty" koanf:"settings"`
}

type RailMerchantAccountSignerConfig struct {
	Mode string `yaml:"mode,omitempty" koanf:"mode"`
	Key  string `yaml:"key,omitempty" koanf:"key"`
}

// MerchantManifestReconcileOptions selects the apply tier (#527). The default
// (both false) is additive + seed-once. Startup provisioning always uses the
// default; the destructive tiers are opt-in via the CLI and never run on boot.
type MerchantManifestReconcileOptions struct {
	// Insert creates missing merchant/issuer/profile/provider-account/secret
	// state declared by the manifest. Manual CLI runs default to plan-only until
	// this or another mutation flag is set.
	Insert bool
	// Overwrite re-asserts manifest values over existing state. Without it,
	// SECRETS are seed-once: a secret already present is left untouched, so a
	// value rotated out of band (via the admin API) is never reverted to the
	// manifest seed. Merchant/issuer/profile are idempotently ensured either
	// way (they are declarative identity, not rotated out of band).
	Overwrite bool
	// Prune deletes secrets that exist for a manifest merchant but are absent
	// from the manifest, reconciling the secret set to the file. Provider-account
	// and issuer removal stay reversible/manual and are not pruned here.
	Prune bool
	// SecretStore overrides where manifest secrets reconcile to. MODE 1 (#723)
	// boot paths pass the runtime's in-memory manifest plane
	// (ManifestSecretStore.Seeder()); nil selects by mode — api mode builds the
	// persistent backend, manifest mode uses an EPHEMERAL in-memory store
	// (validation only; the long-running server seeds its own plane at boot).
	SecretStore merchants.MerchantSecretStore
	// IdentityResolver is an optional test/embedding seam for provider account
	// discovery. Production uses the default resolver over provider read-only
	// identity APIs.
	IdentityResolver ManifestProviderIdentityResolver
	// NMIProbeV5BaseURL is a test-only seam: overrides the base URL the #348
	// test_mode arm-time probe (probeNMIAccountBeforeArm) hits, so tests can
	// point it at a fake gateway instead of the real NMI API. Empty in
	// production — the probe uses nmi.NewClient's documented default.
	NMIProbeV5BaseURL string
}

type ManifestProviderIdentityResolver interface {
	ResolveManifestRailMerchantAccount(ctx context.Context, cfg *config.Config, rail, environment string, account ProviderRailAccountConfig, secrets manifestSecretValues) (manifestProviderIdentity, error)
}

type manifestProviderIdentity struct {
	AccountID   string
	DisplayName *string
	Evidence    map[string]any
}

func (o MerchantManifestReconcileOptions) HasMutations() bool {
	return o.Insert || o.Overwrite || o.Prune
}

// ProvisionMerchant is the single OpenRails merchant-provisioning boundary
// (#527). Standalone calls it with a control plane, which creates/ensures the
// AuthKit permission-group and optional issuer-as-owner before recording
// permission_group_id. Embedded calls it with only Database, which registers an
// ownerless merchant row and applies the same profile/provider-account
// configuration path without touching AuthKit or startup bootstrap markers.
type ProvisionMerchantRequest struct {
	Config        *config.Config
	ControlPlane  *controlplane.ControlPlane
	Database      *db.DB
	SecretStore   merchants.MerchantSecretStore
	SolanaTransit solana.TransitClient
	Slug          string
	Merchant      MerchantConfig
	Options       MerchantManifestReconcileOptions
}

// ReconcileMerchantManifestData provisions merchants and issuer ownership
// declared by a merchant config manifest. It is the single
// merchant-provisioning entry point for push-merchant-config and embedded
// startup paths. Issuer registration is declarative and does not fetch the
// JWKS, so it succeeds even when the issuer's app is not yet running.
func ReconcileMerchantManifestData(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, manifest *BillingConfig, opts MerchantManifestReconcileOptions) error {
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("merchant bootstrap manifest configured but control plane is not enabled")
	}
	if manifest == nil {
		return fmt.Errorf("merchant bootstrap manifest is required")
	}
	if manifest.Version != BootstrapManifestVersion {
		return fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	if err := lockMerchantManifestBootstrap(ctx, cp); err != nil {
		return err
	}
	defer unlockMerchantManifestBootstrap(context.Background(), cp)

	if len(manifest.Merchants) == 0 {
		log.Info("merchant bootstrap manifest has no merchants")
		return nil
	}

	secretStore, solanaTransit, err := manifestReconcileSecretStore(ctx, cfg, cp, opts)
	if err != nil {
		return err
	}
	database, err := db.NewWithPGXPool(cp.Pool().Raw(), cp.Pool().Schema())
	if err != nil {
		return fmt.Errorf("wrap control-plane db: %w", err)
	}

	for _, slug := range sortedMerchantKeys(manifest.Merchants) {
		mt := manifest.Merchants[slug]
		tn, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{
			Config:        cfg,
			ControlPlane:  cp,
			Database:      database,
			SecretStore:   secretStore,
			SolanaTransit: solanaTransit,
			Slug:          slug,
			Merchant:      mt,
			Options:       opts,
		})
		if err != nil {
			return err
		}
		log.WithFields(log.Fields{
			"merchant":    tn.Slug,
			"merchant_id": tn.ID.String(),
		}).Info("merchant bootstrap: merchant ensured")
	}

	// #480/#481: issuer/JWKS trust is AuthKit's remote_application registry (#74),
	// not an OpenRails-owned table — the manifest no longer reconciles issuers.
	return nil
}

// manifestReconcileSecretStore picks where manifest secrets land (#723):
// injected store (mode-1 boot plane) > mode-2 persistent backend > mode-1
// ephemeral memory (CLI runs: DB projections converge, secrets validate but
// are NOT persisted — the running server holds its own from its boot manifest).
func manifestReconcileSecretStore(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, opts MerchantManifestReconcileOptions) (merchants.MerchantSecretStore, solana.TransitClient, error) {
	if opts.SecretStore != nil {
		transitStore, err := merchantsecrets.BuildTransit(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("merchant bootstrap: %w", err)
		}
		return opts.SecretStore, transitStore.SolanaTransit, nil
	}
	if cfg.IsManifestMerchantSource() {
		log.Info("merchant bootstrap: merchant_source=manifest — DB projections reconcile; secrets validate in memory only and are NOT persisted (#723: the server loads them from its boot manifest)")
		transitStore, err := merchantsecrets.BuildTransit(ctx, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("merchant bootstrap: %w", err)
		}
		return merchants.NewMemorySecretStore(), transitStore.SolanaTransit, nil
	}
	secretBackend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	if err != nil {
		return nil, nil, fmt.Errorf("merchant bootstrap: build secret store: %w", err)
	}
	return secretBackend.Secrets, secretBackend.SolanaTransit, nil
}

func ProvisionMerchant(ctx context.Context, req ProvisionMerchantRequest) (*merchants.Merchant, error) {
	slug := merchant.NormalizeSlug(req.Slug)
	mt := req.Merchant
	// MODE 1 (#723): the YAML is the truth — it steamrolls the DB projections
	// and the in-memory secret plane on every apply. Seed-once/plan tiers are
	// mode-2 (api) semantics; forcing here keeps every mode-1 caller (embedded
	// UpsertMerchantConfig, standalone boot, CLI) converging identically.
	if req.Config.IsManifestMerchantSource() {
		req.Options.Insert = true
		req.Options.Overwrite = true
		// Prune needs a store to list; a storeless call (read-side bind with no
		// accounts, e.g. hentai0's empty config) has nothing to prune.
		req.Options.Prune = req.SecretStore != nil
	}
	database := req.Database
	if database == nil {
		if req.ControlPlane == nil || req.ControlPlane.Pool() == nil {
			return nil, fmt.Errorf("merchant provisioning requires database or control plane")
		}
		var err error
		database, err = db.NewWithPGXPool(req.ControlPlane.Pool().Raw(), req.ControlPlane.Pool().Schema())
		if err != nil {
			return nil, fmt.Errorf("wrap control-plane db: %w", err)
		}
	}

	tn, found, err := lookupManifestMerchant(ctx, database, slug)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if !found {
		if !req.Options.Insert {
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
		}
		tn, err = provisionMerchantIdentity(ctx, req.Config, database, req.ControlPlane, slug, mt)
		if err != nil {
			return nil, err
		}
	} else if req.ControlPlane != nil && mt.RemoteApplication != nil && req.Options.Overwrite {
		if _, err := provisionMerchantGroup(ctx, req.ControlPlane, slug, mt); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: update merchant group/remote_application for %q: %w", slug, err)
		}
	}

	// Keep an existing merchant's display name in sync with the manifest (the
	// create path already set it via RegisterMerchant). Idempotent upsert; an
	// empty manifest display name leaves the stored one untouched (COALESCE).
	if found && strings.TrimSpace(mt.DisplayName) != "" {
		if _, err := db.RegisterMerchant(ctx, database.Qx(ctx), db.RegisterMerchantOptions{Slug: slug, DisplayName: mt.DisplayName}); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: sync display name for %q: %w", slug, err)
		}
	}

	if err := reconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, slug, mt, req.SecretStore, req.SolanaTransit, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, cfg *config.Config, database *db.DB, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (*merchants.Merchant, error) {
	if cp == nil {
		// Embedded: OpenRails runs no AuthKit, so it creates/records no permission-group.
		// The merchant's permission-group is the host's AuthKit permission-group of the SAME slug
		// (#541 — merchant slug == group slug); permission_group_id stays NULL here and
		// is set only in standalone, where OpenRails owns the group.
		id, err := db.RegisterMerchant(ctx, database.Qx(ctx), db.RegisterMerchantOptions{Slug: slug, DisplayName: mt.DisplayName})
		if err != nil {
			return nil, err
		}
		tn, found, err := lookupManifestMerchant(ctx, database, slug)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("merchant bootstrap: registered merchant %q but could not read it back", slug)
		}
		tn.ID = id
		return tn, nil
	}

	// #567: the merchant IS a top-level permission-group (child of root). When
	// the merchant declares a remote_application, AuthKit nests it under the
	// merchant group with the `owner` role so host-app delegated tokens
	// administer this merchant only.
	groupID, err := provisionMerchantGroup(ctx, cp, slug, mt)
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision merchant group/remote_application for %q: %w", slug, err)
	}
	svc, err := merchants.NewService(cp.Pool(), nil, config.ExpectedProviderEnvironment(cfg != nil && cfg.IsTestMode()))
	if err != nil {
		return nil, err
	}
	tn, _, err := svc.Provision(ctx, merchants.ProvisionRequest{
		Slug:              slug,
		PermissionGroupID: groupID,
	})
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: provision %q: %w", slug, err)
	}
	return tn, nil
}

func lookupManifestMerchant(ctx context.Context, database *db.DB, slug string) (*merchants.Merchant, bool, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return nil, false, fmt.Errorf("merchant slug is required")
	}
	var (
		id                string
		status            string
		permissionGroupID *string
	)
	err := database.Qx(ctx).QueryRow(ctx, `
		SELECT id::text, status, permission_group_id
		  FROM openrails.merchants
		 WHERE slug = $1
	`, slug).Scan(&id, &status, &permissionGroupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	merchantID, err := merchant.ParseID(id)
	if err != nil {
		return nil, false, err
	}
	owner := ""
	if permissionGroupID != nil {
		owner = *permissionGroupID
	}
	return &merchants.Merchant{
		ID:                merchantID,
		Slug:              slug,
		Status:            merchants.MerchantStatus(status),
		PermissionGroupID: owner,
	}, true, nil
}

func sortedMerchantKeys(in map[string]MerchantConfig) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type railMerchantAccountEntry struct {
	key    string
	rail   string
	config ProviderRailAccountConfig
}

func pspEntries(in map[string]PSPConfig) []railMerchantAccountEntry {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]railMerchantAccountEntry, 0, len(in))
	for _, key := range keys {
		rails := make([]string, 0, len(in[key]))
		for rail := range in[key] {
			rails = append(rails, rail)
		}
		sort.Strings(rails)
		for _, rail := range rails {
			out = append(out, railMerchantAccountEntry{key: key, rail: rail, config: in[key][rail]})
		}
	}
	return out
}

func reconcileManifestMerchantConfiguration(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, slug string, mt MerchantConfig, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
	mctx := merchant.WithID(ctx, merchantID)
	// #850: declared api_host is asserted on every apply (declarative identity,
	// like display_name — not seed-once); omitted leaves the stored value
	// untouched, so a host assigned via the merchant-admin route survives.
	if host := merchants.NormalizeAPIHost(mt.APIHost); host != "" {
		dir, err := merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return fmt.Errorf("api_host %q: %w", host, err)
		}
		if err := dir.SetHostConfig(ctx, merchantID, host); err != nil {
			return fmt.Errorf("set api_host %q: %w", host, err)
		}
	}
	// Apply the merchant_configurations payload (#646): profile, invoice/collection
	// policy, and delegated-invoker abuse windows. Load once, mutate only the
	// declared parts (omit = leave-as-is), upsert if anything changed.
	if hasManifestProfile(mt.Profile) || mt.Invoice != nil || len(mt.DelegatedInvokerWastedSpendWindows) > 0 || len(mt.CheckoutRouting) > 0 {
		store := merchantconfig.NewStore(database)
		conf, _, err := store.Get(mctx)
		if err != nil {
			return fmt.Errorf("load merchant configuration: %w", err)
		}
		if hasManifestProfile(mt.Profile) {
			conf.Profile = models.MerchantProfileConfiguration{
				DisplayName: strings.TrimSpace(mt.Profile.DisplayName),
				LogoURL:     strings.TrimSpace(mt.Profile.LogoURL),
				FromEmail:   strings.TrimSpace(mt.Profile.FromEmail),
				SupportURL:  strings.TrimSpace(mt.Profile.SupportURL),
				SignupURL:   strings.TrimSpace(mt.Profile.SignupURL),
			}
			if conf.Profile.DisplayName == "" {
				conf.Profile.DisplayName = strings.TrimSpace(mt.DisplayName)
			}
		}
		if mt.Invoice != nil {
			if mt.Invoice.CollectionThreshold != nil {
				conf.InvoiceCollectionThreshold = mt.Invoice.CollectionThreshold
			}
			if mt.Invoice.MonthlyFloor != nil {
				conf.InvoiceMonthlyFloor = mt.Invoice.MonthlyFloor
			}
			if b := strings.TrimSpace(mt.Invoice.BillingPeriodBoundary); b != "" {
				conf.InvoiceBillingBoundary = b
			}
			if mt.Invoice.DelinquencyGraceDays != nil {
				conf.ArrearsGraceDays = mt.Invoice.DelinquencyGraceDays
			}
			if mt.Invoice.DelinquencyAmountFloor != nil {
				conf.ArrearsDelinquencyFloor = mt.Invoice.DelinquencyAmountFloor
			}
		}
		if len(mt.DelegatedInvokerWastedSpendWindows) > 0 {
			windows := make([]models.BudgetWindowPolicy, 0, len(mt.DelegatedInvokerWastedSpendWindows))
			for _, w := range mt.DelegatedInvokerWastedSpendWindows {
				d, err := time.ParseDuration(strings.TrimSpace(w.Window))
				if err != nil {
					return fmt.Errorf("delegated_invoker_wasted_spend_windows %q: window: %w", w.Key, err)
				}
				windows = append(windows, models.BudgetWindowPolicy{
					Key:           strings.TrimSpace(w.Key),
					WindowSeconds: int64(d / time.Second),
					Limit:         w.Limit,
					Currency:      strings.TrimSpace(w.Currency),
				})
			}
			conf.DelegatedInvokerWastedSpendWindows = windows
		}
		if len(mt.CheckoutRouting) > 0 {
			// or#288: the declared order IS the policy, so it replaces whole.
			routing, err := merchantconfig.NormalizeCheckoutRouting(checkoutRoutingRules(mt.CheckoutRouting))
			if err != nil {
				return err
			}
			conf.CheckoutRouting = routing
		}
		if err := store.Upsert(mctx, conf); err != nil {
			return fmt.Errorf("upsert merchant configuration: %w", err)
		}
	}

	for _, entry := range pspEntries(mt.PSPs) {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: provider account secrets require a secret store")
		}
		if err := reconcileManifestRailMerchantAccount(ctx, cfg, database, merchantID, slug, entry.key, entry.rail, entry.config, secretStore, transit, opts); err != nil {
			return err
		}
	}
	if opts.Prune {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: prune requires a secret store")
		}
		if err := pruneManifestSecrets(ctx, cfg, merchantID, mt, secretStore); err != nil {
			return err
		}
	}
	return nil
}

// pruneManifestSecrets deletes secrets held for the merchant that the manifest
// no longer declares (#527 --prune), reconciling the stored secret set to the
// file. Names are derived exactly as Put derives them.
func pruneManifestSecrets(ctx context.Context, cfg *config.Config, merchantID merchant.ID, mt MerchantConfig, secretStore merchants.MerchantSecretStore) error {
	declared := map[string]struct{}{}
	for _, entry := range pspEntries(mt.PSPs) {
		rail := normalizeManifestRail(entry.rail)
		environment, err := manifestProviderEnvironment(cfg, entry.config)
		if err != nil {
			return err
		}
		accountID := strings.TrimSpace(entry.config.AccountID)
		if accountID == "" {
			if rail != string(models.RailSolana) {
				return fmt.Errorf("provider account %q account_id is required before pruning secrets", rail)
			}
			if len(entry.config.Secrets) == 0 {
				continue
			}
			secrets, err := newManifestSecretValues(rail, entry.config.Secrets)
			if err != nil {
				return err
			}
			if _, ok := secrets.sources["private_key"]; !ok {
				return fmt.Errorf("provider account %q private_key is required before pruning secrets without account_id", rail)
			}
			accountID, err = solanaLocalKeypairPublicKey(secrets)
			if err != nil {
				return err
			}
		}
		for key := range entry.config.Secrets {
			name, err := merchants.PSPSecretName(rail, environment, accountID, key)
			if err != nil {
				return err
			}
			declared[name] = struct{}{}
		}
	}
	existing, err := secretStore.List(ctx, merchantID)
	if err != nil {
		return fmt.Errorf("list merchant secrets for prune: %w", err)
	}
	for _, name := range existing {
		if _, ok := declared[name]; ok {
			continue
		}
		if err := secretStore.Delete(ctx, merchantID, name); err != nil {
			return fmt.Errorf("prune secret %s: %w", name, err)
		}
		log.WithField("secret", name).Info("merchant bootstrap: pruned secret absent from manifest")
	}
	return nil
}

func hasManifestProfile(p MerchantProfileConfig) bool {
	return strings.TrimSpace(p.DisplayName) != "" ||
		strings.TrimSpace(p.LogoURL) != "" ||
		strings.TrimSpace(p.FromEmail) != "" ||
		strings.TrimSpace(p.SupportURL) != "" ||
		strings.TrimSpace(p.SignupURL) != ""
}

// resolvedManifestRailAccount is the store/DB-independent front half of a
// manifest rail-account reconcile: normalized rail, resolved environment,
// account identity and secret values, fully validated. Shared by the DB
// reconcile path and the seeding-only plane build (#723).
type resolvedManifestRailAccount struct {
	rail           string
	environment    string
	accountID      string
	secrets        manifestSecretValues
	identity       manifestProviderIdentity
	signerEvidence map[string]string
}

func resolveManifestRailAccount(ctx context.Context, cfg *config.Config, rail string, account ProviderRailAccountConfig, transit solana.TransitClient, resolver ManifestProviderIdentityResolver) (resolvedManifestRailAccount, error) {
	out := resolvedManifestRailAccount{rail: normalizeManifestRail(rail)}
	if out.rail == "" {
		return out, fmt.Errorf("provider account rail is required")
	}
	environment, err := manifestProviderEnvironment(cfg, account)
	if err != nil {
		return out, err
	}
	out.environment = environment
	// #711: the Solana runtime knobs live in the account settings block —
	// validate strictly at push time so a typo'd key/value fails loudly here
	// instead of being stored inert.
	if out.rail == string(models.RailSolana) {
		if err := config.ValidateSolanaAccountSettings(account.Settings); err != nil {
			return out, fmt.Errorf("provider account %q: %w", out.rail, err)
		}
		// or#881: the token declaration is resolved against the built-in mint
		// registry for THIS account's network, so a restated built-in mint or a
		// custom token with no mint fails the push instead of at arm time.
		settings, err := config.ParseSolanaAccountSettings(account.Settings)
		if err != nil {
			return out, fmt.Errorf("provider account %q: %w", out.rail, err)
		}
		if _, err := solanatokens.ResolveDeclared(manifestSolanaNetwork(environment), settings.Tokens); err != nil {
			return out, fmt.Errorf("provider account %q: %w", out.rail, err)
		}
	}
	// or#879: custody is a MODIFIER on any rail, so its settings are validated
	// for every PSP — a typo'd or retired custody key fails the push loudly
	// instead of being stored inert on a money path.
	custody, err := config.ParseCustodySettings(account.Settings)
	if err != nil {
		return out, fmt.Errorf("provider account %q: %w", out.rail, err)
	}
	if custody.ThirdParty() && out.rail != string(models.RailNMI) {
		return out, fmt.Errorf("provider account %q: custodian %q is not supported on this rail — only nmi has a detokenizing proxy charge path", out.rail, custody.Custodian)
	}
	secrets, err := newManifestSecretValues(out.rail, account.Secrets)
	if err != nil {
		return out, err
	}
	out.secrets = secrets
	if resolver == nil {
		resolver = defaultManifestProviderIdentityResolver{}
	}
	identity, err := resolver.ResolveManifestRailMerchantAccount(ctx, cfg, out.rail, environment, account, secrets)
	if err != nil {
		return out, err
	}
	out.identity = identity
	accountID := strings.TrimSpace(identity.AccountID)
	// For Solana, manifestProviderSignerEvidence derives the stored provider-account
	// identity from the signer key; a declared account_id is ignored (warned).
	signerEvidence, accountID, err := manifestProviderSignerEvidence(ctx, out.rail, accountID, account, secrets, transit)
	if err != nil {
		return out, err
	}
	out.signerEvidence = signerEvidence
	if accountID == "" {
		if out.rail == string(models.RailSolana) {
			return out, fmt.Errorf("provider account %q requires signer-derived identity", out.rail)
		}
		return out, fmt.Errorf("provider account %q requires account_id", out.rail)
	}
	// #697: rail-specific format doctrine (CCBill ids are dash-joined).
	if err := config.ValidateRailAccountID(models.Rail(out.rail), accountID); err != nil {
		return out, fmt.Errorf("provider account %q: %w", out.rail, err)
	}
	out.accountID = accountID
	return out, nil
}

// SeedMerchantManifestSecretPlane resolves ONE merchant's manifest-declared
// rail secrets and seeds them into store, touching no DB state — the same
// values the server's boot reconcile seeds into its runtime plane. MODE-1
// one-off processes (pull-provider CLI, #723) build their ephemeral in-memory
// plane through it and arm per-merchant fetchers from the on-disk manifest.
func SeedMerchantManifestSecretPlane(ctx context.Context, cfg *config.Config, merchantID merchant.ID, mt MerchantConfig, store merchants.MerchantSecretStore, transit solana.TransitClient) error {
	if store == nil {
		return fmt.Errorf("merchant manifest secret plane: store is required")
	}
	for _, entry := range pspEntries(mt.PSPs) {
		ra, err := resolveManifestRailAccount(ctx, cfg, entry.rail, entry.config, transit, nil)
		if err != nil {
			return err
		}
		for key, fallback := range entry.config.Secrets {
			name, err := merchants.PSPSecretName(ra.rail, ra.environment, ra.accountID, key)
			if err != nil {
				return err
			}
			value, err := ra.secrets.Resolve(key, fallback)
			if err != nil {
				return fmt.Errorf("resolve secret %s.%s: %w", ra.rail, key, err)
			}
			if _, err := store.Put(ctx, merchantID, name, value); err != nil {
				return fmt.Errorf("seed secret %s: %w", name, err)
			}
		}
	}
	return nil
}

func reconcileManifestRailMerchantAccount(ctx context.Context, cfg *config.Config, database *db.DB, merchantID merchant.ID, merchantSlug, localKey, rail string, account ProviderRailAccountConfig, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
	ra, err := resolveManifestRailAccount(ctx, cfg, rail, account, transit, opts.IdentityResolver)
	if err != nil {
		return err
	}
	rail = ra.rail
	environment := ra.environment
	accountID := ra.accountID
	secrets := ra.secrets
	identity := ra.identity
	signerEvidence := ra.signerEvidence
	for key, value := range account.Secrets {
		name, err := merchants.PSPSecretName(rail, environment, accountID, key)
		if err != nil {
			return err
		}
		_, gerr := secretStore.Get(ctx, merchantID, name)
		switch {
		case gerr == nil && !opts.Overwrite:
			// Seed-once (#527): unless --overwrite, leave an already-present secret
			// untouched so a value rotated out of band is never reverted to the seed.
			continue
		case errors.Is(gerr, merchants.ErrSecretNotFound) && !opts.Insert:
			return fmt.Errorf("secret %s is missing; rerun with --insert to create it", name)
		case gerr != nil && !errors.Is(gerr, merchants.ErrSecretNotFound):
			return fmt.Errorf("check secret %s: %w", name, gerr)
		}
		value, err := secrets.Resolve(key, value)
		if err != nil {
			return fmt.Errorf("resolve secret %s.%s: %w", rail, key, err)
		}
		if _, err := secretStore.Put(ctx, merchantID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
	}
	// #348, reinstated at arm time (#788 deleted the boot-time client map this
	// used to probe, with no replacement — refusing an armed live NMI account
	// under test_mode). Runs before anything below persists the account row.
	if rail == string(models.RailNMI) && cfg != nil && cfg.IsTestMode() {
		if err := probeNMIAccountBeforeArm(ctx, database, secretStore, merchantID, rail, environment, accountID, opts.NMIProbeV5BaseURL); err != nil {
			return err
		}
	}
	reconcileStripeWebhook := func() error {
		if rail != string(models.RailStripe) {
			return nil
		}
		res, err := catalog.ReconcileManagedStripeWebhook(ctx, catalog.ManagedStripeWebhookParams{
			Config:              cfg,
			SecretStore:         secretStore,
			MerchantID:          merchantID,
			MerchantSlug:        merchantSlug,
			ProviderEnvironment: environment,
			PspID:               accountID,
			EnabledEvents:       webhooks.HandledStripeEventTypes,
		})
		if err != nil {
			return fmt.Errorf("reconcile stripe webhook endpoint: %w", err)
		}
		fields := log.Fields{"merchant": merchantSlug, "stripe_account_id": accountID}
		if res.Skipped {
			fields["reason"] = res.SkipReason
			log.WithFields(fields).Info("merchant bootstrap: stripe webhook endpoint reconcile skipped")
		} else {
			fields["action"] = res.Result.Action
			fields["endpoint_id"] = res.Result.EndpointID
			log.WithFields(fields).Info("merchant bootstrap: stripe webhook endpoint reconciled")
		}
		return nil
	}

	found := false
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		_, err := database.Gen(ctx).GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{
			MerchantID:  merchantID.UUID(),
			Rail:        rail,
			Environment: stringPtrIfNotEmpty(environment),
			AccountID:   accountID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	}); err != nil {
		return fmt.Errorf("lookup provider account %s:%s:%s: %w", rail, environment, accountID, err)
	}
	if !found && !opts.Insert {
		return fmt.Errorf("provider account %s:%s:%s is missing; rerun with --insert to create it", rail, environment, accountID)
	}
	if found && !opts.Overwrite {
		return reconcileStripeWebhook()
	}
	displayName := identity.DisplayName
	if n := strings.TrimSpace(localKey); n != "" {
		displayName = &n
	}
	evidence := identity.Evidence
	if evidence == nil {
		evidence = map[string]any{"source": "merchant_config_manifest"}
	}
	if signerEvidence != nil {
		evidence["signer"] = signerEvidence
	}
	if len(account.Settings) > 0 {
		evidence["settings"] = account.Settings
	}
	evidenceJSON, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("encode provider account evidence: %w", err)
	}
	// #650: a provider account belongs to exactly one merchant. Fail with a clear
	// error if another merchant already owns this identity, rather than letting the
	// global-uniqueness upsert reject it with an opaque unique-violation under RLS.
	if err := merchants.AssertPSPUnowned(ctx, gen.New(database.Pool()), merchantID.UUID(), rail, environment, accountID); err != nil {
		return err
	}
	mctx := merchant.WithID(ctx, merchantID)
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		// #662: derive the id from the global natural key and store the SAME
		// normalized (rail, environment, account_id) it is hashed from.
		railAcctID, nRail, nEnv, nAccount := merchants.PSPNaturalKey(rail, environment, accountID)
		_, err := database.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
			ID:          railAcctID,
			MerchantID:  merchantID.UUID(),
			Rail:        nRail,
			Environment: &nEnv,
			AccountID:   nAccount,
			Key:         displayName,
			Archived:    &account.Archived,
			Evidence:    evidenceJSON,
		})
		if err != nil {
			return fmt.Errorf("upsert provider account %s:%s: %w", rail, accountID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return reconcileStripeWebhook()
}

// probeNMIAccountBeforeArm reinstates #348 at the manifest arm boundary: a
// test_mode deployment must never arm an NMI account whose credentials
// actually belong to a LIVE gateway — every charge through it would move
// real money while the operator believes the account is sandboxed. It reads
// the account's EFFECTIVE security_key (whatever this reconcile pass just
// wrote, or — under seed-once — whatever was already stored) and, cache-first
// (internal/integrations/nmi.CheckTestModeArm, #348's cooldown), probes the
// live gateway with the documented non-issued test card.
//
// Posture, preserved exactly from #348's original boot probe: only a
// conclusive "live" verdict refuses. A probe error (offline dev, bad
// credentials, transport failure — nmi.ArmDecision.ProbeErr) is
// indeterminate and only warns; it is NEVER fail-closed, because network or
// credential noise is not evidence of a live account. Production deployments
// (test_mode=false) never reach this at all — probing charges a real card.
func probeNMIAccountBeforeArm(ctx context.Context, database *db.DB, secretStore merchants.MerchantSecretStore, merchantID merchant.ID, rail, environment, accountID, probeV5BaseURL string) error {
	name, err := merchants.PSPSecretName(rail, environment, accountID, "security_key")
	if err != nil {
		return err
	}
	sec, err := secretStore.Get(ctx, merchantID, name)
	if errors.Is(err, merchants.ErrSecretNotFound) {
		return nil // unconfigured; nothing to verify
	}
	if err != nil {
		return fmt.Errorf("provider account %s:%s:%s: read security_key for test_mode probe: %w", rail, environment, accountID, err)
	}
	securityKey := strings.TrimSpace(sec.Value)
	if securityKey == "" {
		return nil
	}
	client, err := nmi.NewClient(accountID, &config.NMIProviderSettings{SecurityKey: securityKey}, true)
	if err != nil {
		return nil // never stricter than #348: a client that fails to construct cannot be probed
	}
	if probeV5BaseURL != "" {
		client.V5BaseURL = probeV5BaseURL
	}
	// Scoped by merchant + rail + environment + account so a cached verdict
	// never answers for a different merchant's account (#348's original cache
	// had exactly one deployment-wide credential set to consider; this one
	// has many).
	cacheKey := merchantID.String() + ":" + name
	// probe_verdicts is one of the four RLS-EXEMPT tables (with merchants,
	// worker_health and destructive_action_switch), so the base pool genuinely
	// answers here — unlike the sites or#824's sweep found. The cache key
	// carries the merchant, so scope is not lost.
	decision := nmi.CheckTestModeArm(ctx, database.GenDirectory(), client, cacheKey)
	if decision.ProbeErr != nil {
		log.WithError(decision.ProbeErr).WithFields(log.Fields{
			"merchant_id": merchantID.String(), "rail": rail, "account_id": accountID,
		}).Warnf("⚠️  provider account %s:%s: could not verify the NMI account is a sandbox account; proceeding, but confirm the credentials before relying on test_mode (#348)", rail, accountID)
		return nil
	}
	if !decision.Refuse {
		log.WithFields(log.Fields{
			"merchant_id": merchantID.String(), "rail": rail, "account_id": accountID,
		}).Info("provider account: NMI account verified as simulating (test env, #348)")
		return nil
	}
	if decision.Cached {
		return fmt.Errorf("provider account %s:%s:%s: PRODUCTION NMI credentials detected while test_mode is enabled — cached probe verdict 'live' from %s (within the %s cooldown; not re-probing); refusing to arm (use the sandbox account credentials, rotate the key, or unset test_mode) (#348)",
			rail, environment, accountID, decision.CheckedAt.UTC().Format(time.RFC3339), nmi.ProbeVerdictCooldown)
	}
	return fmt.Errorf("provider account %s:%s:%s: PRODUCTION NMI credentials detected while test_mode is enabled — the account did not simulate the test-card probe, so real charges could occur; refusing to arm (use the sandbox account credentials, or unset test_mode) (#348)",
		rail, environment, accountID)
}

// manifestProviderSignerEvidence validates the Solana signer and returns signer
// evidence plus the derived provider-account identity. Solana never needs
// account_id — the stored DB identity is always the signer public key; a declared
// value is ignored (warned).
func manifestProviderSignerEvidence(ctx context.Context, rail, accountID string, account ProviderRailAccountConfig, secrets manifestSecretValues, transit solana.TransitClient) (map[string]string, string, error) {
	if rail == string(models.RailSolana) && strings.TrimSpace(accountID) != "" {
		log.Warnf("solana provider account: declared account_id %s is ignored; it is always derived from the signer's public key", strings.TrimSpace(accountID))
		accountID = ""
	}
	if account.Signer == nil {
		if _, ok := secrets.sources["private_key"]; ok && rail == string(models.RailSolana) {
			pub, err := solanaLocalKeypairPublicKey(secrets)
			if err != nil {
				return nil, "", err
			}
			return map[string]string{"mode": "local_keypair"}, pub, nil
		}
		return nil, accountID, nil
	}
	if rail != string(models.RailSolana) {
		return nil, "", fmt.Errorf("provider account signer is only supported for solana")
	}
	mode := strings.ToLower(strings.TrimSpace(account.Signer.Mode))
	switch mode {
	case "local_keypair":
		if _, ok := secrets.sources["private_key"]; !ok {
			return nil, "", fmt.Errorf("solana signer mode local_keypair requires secrets.private_key")
		}
		if strings.TrimSpace(account.Signer.Key) != "" {
			return nil, "", fmt.Errorf("solana signer mode local_keypair must not set key")
		}
		pub, err := solanaLocalKeypairPublicKey(secrets)
		if err != nil {
			return nil, "", err
		}
		return map[string]string{"mode": "local_keypair"}, pub, nil
	case "vault_transit":
		if _, ok := secrets.sources["private_key"]; ok {
			return nil, "", fmt.Errorf("solana signer mode vault_transit cannot also set secrets.private_key")
		}
		key := strings.TrimSpace(account.Signer.Key)
		if key == "" {
			return nil, "", fmt.Errorf("solana signer mode vault_transit requires key")
		}
		if transit == nil {
			return nil, "", fmt.Errorf("solana signer mode vault_transit requires a Vault connection (vault.enabled with reachable Transit)")
		}
		pub, err := solanaTransitPublicKey(ctx, transit, key)
		if err != nil {
			return nil, "", err
		}
		return map[string]string{"mode": "vault_transit", "key": key}, pub, nil
	default:
		return nil, "", fmt.Errorf("solana signer mode must be local_keypair or vault_transit")
	}
}

// solanaLocalKeypairPublicKey parses the base58 private_key secret and returns its
// Solana address (base58 public key).
func solanaLocalKeypairPublicKey(secrets manifestSecretValues) (string, error) {
	raw, ok, err := secrets.ResolveIfPresent("private_key")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("solana signer mode local_keypair requires secrets.private_key")
	}
	key, err := solanago.PrivateKeyFromBase58(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("solana signer mode local_keypair private_key: %w", err)
	}
	return key.PublicKey().String(), nil
}

// solanaTransitPublicKey reads the Vault Transit Ed25519 key's public key and
// returns its Solana address (base58). The private key never leaves Vault.
func solanaTransitPublicKey(ctx context.Context, transit solana.TransitClient, key string) (string, error) {
	raw, err := transit.PublicKey(ctx, key)
	if err != nil {
		return "", fmt.Errorf("solana vault transit signer %q public key: %w", key, err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("solana vault transit signer %q public key is %d bytes, want 32", key, len(raw))
	}
	return solanago.PublicKeyFromBytes(raw).String(), nil
}

// manifestProviderEnvironment derives a PSP's environment from deployment
// posture (#681/#882 — test under test_mode, live otherwise). It is never
// declared: a manifest that still carries `environment:` fails loudly here.
func manifestProviderEnvironment(cfg *config.Config, account ProviderRailAccountConfig) (string, error) {
	if strings.TrimSpace(account.LegacyEnvironment) != "" {
		return "", fmt.Errorf("psp `environment:` is no longer configurable (#882) — it is derived from test_mode (sandbox => test, live => live); remove the key")
	}
	return config.ExpectedProviderEnvironment(cfg != nil && cfg.IsTestMode()), nil
}

// manifestSolanaNetwork maps a PSP environment onto the Solana network the same
// way railresolve does at arm time (#349: network derives from test_mode alone).
func manifestSolanaNetwork(environment string) string {
	if environment == config.ProviderEnvironmentTest {
		return "devnet"
	}
	return "mainnet"
}

func normalizeManifestRail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

type manifestSecretValues struct {
	rail    string
	sources map[string]string
	values  map[string]string
}

func newManifestSecretValues(rail string, sources map[string]string) (manifestSecretValues, error) {
	out := manifestSecretValues{
		rail:    rail,
		sources: map[string]string{},
		values:  map[string]string{},
	}
	for key, value := range sources {
		canonical, err := merchants.NormalizePSPSecretKey(rail, key)
		if err != nil {
			return out, err
		}
		if _, exists := out.sources[canonical]; exists {
			return out, fmt.Errorf("duplicate provider account secret key %q", canonical)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return out, fmt.Errorf("provider account secret %s.%s is empty", rail, canonical)
		}
		out.sources[canonical] = value
	}
	return out, nil
}

func (v manifestSecretValues) Resolve(key string, fallback string) (string, error) {
	canonical, err := merchants.NormalizePSPSecretKey(v.rail, key)
	if err != nil {
		return "", err
	}
	if value, ok := v.values[canonical]; ok {
		return value, nil
	}
	source, ok := v.sources[canonical]
	if !ok {
		source = fallback
	}
	value := strings.TrimSpace(source)
	if value == "" {
		return "", fmt.Errorf("provider account secret %s.%s is empty", v.rail, canonical)
	}
	v.values[canonical] = value
	return value, nil
}

func (v manifestSecretValues) ResolveIfPresent(key string) (string, bool, error) {
	canonical, err := merchants.NormalizePSPSecretKey(v.rail, key)
	if err != nil {
		return "", false, err
	}
	source, ok := v.sources[canonical]
	if !ok {
		return "", false, nil
	}
	value, err := v.Resolve(canonical, source)
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

type defaultManifestProviderIdentityResolver struct{}

func (defaultManifestProviderIdentityResolver) ResolveManifestRailMerchantAccount(ctx context.Context, cfg *config.Config, rail, environment string, account ProviderRailAccountConfig, secrets manifestSecretValues) (manifestProviderIdentity, error) {
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		return manifestProviderIdentity{
			AccountID: accountID,
			Evidence:  map[string]any{"source": "merchant_config_manifest.account_id"},
		}, nil
	}
	if rail == string(models.RailSolana) {
		return manifestProviderIdentity{
			Evidence: map[string]any{"source": "merchant_config_manifest.signer"},
		}, nil
	}
	// Auto-discovery via live credentials was removed (#592): every rail must
	// declare account_id in the manifest.
	return manifestProviderIdentity{}, fmt.Errorf("provider account_id is required for %s (auto-discovery removed; declare account_id in the manifest)", rail)
}

func stringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func remoteApplicationStaticPublicKeys(app *RemoteApplicationConfig) ([]authkit.RemoteAppKey, error) {
	if app == nil || len(app.JWKS.Keys) == 0 {
		return nil, nil
	}
	keys := make([]authkit.RemoteAppKey, 0, len(app.JWKS.Keys))
	for _, raw := range app.JWKS.Keys {
		jwk := raw.authkitJWK()
		pub, err := jwtkit.JWKToPublicKey(jwk)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", strings.TrimSpace(jwk.Kid), err)
		}
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			return nil, fmt.Errorf("key %q: marshal public key: %w", strings.TrimSpace(jwk.Kid), err)
		}
		keys = append(keys, authkit.RemoteAppKey{
			KID:          strings.TrimSpace(jwk.Kid),
			PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		})
	}
	return keys, nil
}

// provisionMerchantGroup ensures the merchant's top-level permission-group exists
// (`type=merchant`, `resourceRef=slug`, child of `root` — #567) and,
// when the merchant declares a remote_application, registers it as a
// remote_application nested under the merchant group and grants it the merchant
// `owner` role (full `merchant:*` authority, scoped to this merchant alone since
// federated authority claims are stripped). Idempotent: re-applying converges the
// group + remote_application state. Returns the merchant group's internal id.
func provisionMerchantGroup(ctx context.Context, cp *controlplane.ControlPlane, slug string, mt MerchantConfig) (string, error) {
	slug = merchant.NormalizeSlug(slug)
	if slug == "" {
		return "", fmt.Errorf("merchant slug is required")
	}
	// #548: validate the merchant slug as a legal slug up front for a clear error.
	if err := merchant.ValidateSlug(slug); err != nil {
		return "", err
	}
	core := cp.Core()
	if core == nil {
		return "", fmt.Errorf("merchant bootstrap: control plane core unavailable")
	}

	// Ensure the root group + containment exist before creating typed groups
	// (concurrent-boot tolerant, #844).
	if err := controlplane.EnsureRootContainment(ctx, core); err != nil {
		return "", fmt.Errorf("merchant bootstrap: %w", err)
	}

	// Idempotently create the merchant permission-group (resolve, else create).
	groupID, err := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
	if errors.Is(err, authkit.ErrGroupNotFound) {
		groupID, err = core.CreatePermissionGroup(ctx, authkit.CreatePermissionGroupRequest{
			Persona:       controlplane.MerchantType,
			InstanceSlug:  slug,
			ParentPersona: authcore.RootPersona,
		})
		if err != nil {
			// #844: concurrent first-create loser — re-read and adopt the
			// winner's group (the Reconcile path holds an advisory lock, but
			// ProvisionMerchant callers do not).
			id, rerr := core.ResolveGroupIDForSlug(ctx, controlplane.MerchantType, slug)
			if rerr != nil {
				return "", fmt.Errorf("merchant bootstrap: create merchant group %q: %w", slug, err)
			}
			groupID = id
		}
	} else if err != nil {
		return "", fmt.Errorf("merchant bootstrap: resolve merchant group %q: %w", slug, err)
	}

	// Register the merchant's federated issuer as a remote_application nested
	// under the merchant group, then grant it the merchant `owner` role.
	if mt.RemoteApplication != nil {
		ra, err := manifestRemoteApplicationToAuthKit(slug, groupID, mt.RemoteApplication)
		if err != nil {
			return "", fmt.Errorf("merchant bootstrap: remote_application for %q: %w", slug, err)
		}
		stored, err := core.UpsertRemoteApplication(ctx, ra)
		if err != nil {
			return "", fmt.Errorf("merchant bootstrap: register remote_application for %q: %w", slug, err)
		}
		if err := core.Genesis().AssignGroupRole(ctx, controlplane.MerchantType, slug, stored.ID, authcore.SubjectKindRemoteApp, controlplane.MerchantRoleOwner); err != nil {
			return "", fmt.Errorf("merchant bootstrap: grant remote_application owner role for %q: %w", slug, err)
		}
	}

	return groupID, nil
}

// manifestRemoteApplicationToAuthKit maps a merchant's manifest remote_application
// onto an AuthKit remote_application registration nested under the merchant group.
func manifestRemoteApplicationToAuthKit(merchantSlug, groupID string, app *RemoteApplicationConfig) (authkit.RemoteApplication, error) {
	appSlug := strings.TrimSpace(app.Slug)
	if appSlug == "" {
		appSlug = merchantSlug + "-app"
	}
	mode := authkit.RemoteAppModeJWKS
	publicKeys := app.PublicKeys
	if len(app.JWKS.Keys) > 0 {
		keys, err := remoteApplicationStaticPublicKeys(app)
		if err != nil {
			return authkit.RemoteApplication{}, err
		}
		publicKeys = keys
	}
	if len(publicKeys) > 0 {
		mode = authkit.RemoteAppModeStatic
	}
	return authkit.RemoteApplication{
		Slug:              appSlug,
		PermissionGroupID: groupID,
		Issuer:            strings.TrimSpace(app.Issuer),
		JWKSURI:           strings.TrimSpace(app.JWKSURI),
		Mode:              mode,
		PublicKeys:        publicKeys,
		Enabled:           true,
	}, nil
}

func lockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) error {
	_, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_lock($1)`, merchantManifestAdvisoryLock)
	if err != nil {
		return fmt.Errorf("merchant bootstrap: acquire advisory lock: %w", err)
	}
	return nil
}

func unlockMerchantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) {
	if _, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_unlock($1)`, merchantManifestAdvisoryLock); err != nil {
		log.WithError(err).Warn("merchant bootstrap: release advisory lock failed")
	}
}
