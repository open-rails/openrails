package merchantbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/service"
)

const DefaultMerchantConfigManifestPath = "/etc/openrails/merchants.yaml"

type BillingConfig struct {
	Version   int                                   `yaml:"version" koanf:"version"`
	Merchants map[string]config.MerchantDeclaration `yaml:"merchants" koanf:"merchants"`
}

// LoadMerchantConfigManifest reads and validates a merchant config manifest.
func LoadMerchantConfigManifest(path string) (*BillingConfig, error) {
	return LoadMerchantConfigManifestFiles(path)
}

// LoadMerchantConfigManifestFiles loads a merchant config YAML file plus optional
// structured YAML overlays through koanf, then validates the merged tree.
func LoadMerchantConfigManifestFiles(path string, overlays ...string) (*BillingConfig, error) {
	docs := make([][]byte, 0, 1+len(overlays))
	for _, p := range append([]string{path}, overlays...) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p) // #nosec G304 -- operator-supplied manifest path
		if err != nil {
			return nil, fmt.Errorf("read merchant config manifest %s: %w", p, err)
		}
		docs = append(docs, raw)
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("read merchant config manifest: no path given")
	}
	return LoadMerchantConfigManifestWithOverlays(docs[0], docs[1:]...)
}

// ReadMerchantManifestOverlays reads operator-mounted overlay files for
// LoadMerchantConfigManifestWithOverlays. Every listed path must exist.
func ReadMerchantManifestOverlays(paths []string) ([][]byte, error) {
	out := make([][]byte, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p) // #nosec G304 -- operator-configured overlay path
		if err != nil {
			return nil, fmt.Errorf("read merchant manifest overlay %s: %w", p, err)
		}
		out = append(out, raw)
	}
	return out, nil
}

// LoadMerchantConfigManifestWithOverlays merges the manifest with structured YAML
// overlays (later wins) and validates the result. Overlays are the caller's
// concern (a host's mounted secret files); the engine reads no env or files.
func LoadMerchantConfigManifestWithOverlays(raw []byte, overlays ...[]byte) (*BillingConfig, error) {
	k := koanf.New(".")
	for i, doc := range append([][]byte{raw}, overlays...) {
		if len(strings.TrimSpace(string(doc))) == 0 {
			continue
		}
		if i > 0 {
			if err := ValidateMerchantSecretOverlay(doc); err != nil {
				return nil, fmt.Errorf("load merchant config overlay %d: %w", i, err)
			}
		}
		if err := k.Load(rawbytes.Provider(doc), koanfyaml.Parser()); err != nil {
			if i == 0 {
				return nil, fmt.Errorf("load merchant config manifest: %w", err)
			}
			return nil, fmt.Errorf("load merchant config overlay %d: %w", i, err)
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

// ValidateMerchantSecretOverlay limits operator-mounted overlays to processor
// secret values. The checked-in manifest remains the authority for merchant
// identity, account IDs, settings, custodians, and billing policy. Without
// this shape check, koanf's deep merge would let a bad Vault document silently
// replace those structural fields before the strict final parse.
func ValidateMerchantSecretOverlay(raw []byte) error {
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parse merchant secret overlay: %w", err)
	}
	if len(root) == 0 {
		return nil
	}
	for key := range root {
		if key != "merchants" {
			return fmt.Errorf("merchant secret overlay only accepts the merchants.<slug>.psps.<psp>.secrets shape (found %q)", key)
		}
	}
	merchants, ok := root["merchants"].(map[string]any)
	if !ok {
		return fmt.Errorf("merchant secret overlay merchants must be a mapping")
	}
	for slug, rawMerchant := range merchants {
		merchant, ok := rawMerchant.(map[string]any)
		if !ok {
			return fmt.Errorf("merchant secret overlay merchants.%s must be a mapping", slug)
		}
		for key := range merchant {
			if key != "psps" {
				return fmt.Errorf("merchant secret overlay cannot set merchants.%s.%s; structural manifest fields stay in the base manifest", slug, key)
			}
		}
		psps, ok := merchant["psps"].(map[string]any)
		if !ok {
			return fmt.Errorf("merchant secret overlay merchants.%s.psps must be a mapping", slug)
		}
		for psp, rawPSP := range psps {
			declared, ok := rawPSP.(map[string]any)
			if !ok {
				return fmt.Errorf("merchant secret overlay merchants.%s.psps.%s must be a mapping", slug, psp)
			}
			for key := range declared {
				if key != "secrets" {
					return fmt.Errorf("merchant secret overlay cannot set merchants.%s.psps.%s.%s; only secrets are overlayed", slug, psp, key)
				}
			}
			if _, ok := declared["secrets"].(map[string]any); !ok {
				return fmt.Errorf("merchant secret overlay merchants.%s.psps.%s.secrets must be a mapping", slug, psp)
			}
		}
	}
	return nil
}

// ParseMerchantConfigManifest parses the merchant config manifest consumed by
// push-merchant-config. Bootstrap authority and catalog state are intentionally
// rejected by the strict YAML decoder.
func ParseMerchantConfigManifest(raw []byte) (*BillingConfig, error) {
	if err := RejectRenamedMerchantConfigKeys(raw); err != nil {
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

// RejectRenamedMerchantConfigKeys fails a manifest that still spells the
// psps key by a retired name (psps <- accounts <- rail_merchant_accounts <-
// provider_accounts). The strict parser would
// reject these as unknown fields anyway, but "unknown field" reads like a typo
// — a rename deserves a pointer. Silent-ignore is the worst failure here: it
// would apply an empty account set.
func RejectRenamedMerchantConfigKeys(raw []byte) error {
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
		dst.Merchants = map[string]config.MerchantDeclaration{}
	}
	for slug, srcMerchant := range src.Merchants {
		dstMerchant := dst.Merchants[slug]
		MergeMerchantConfig(&dstMerchant, srcMerchant)
		dst.Merchants[slug] = dstMerchant
	}
}

func MergeMerchantConfig(dst *config.MerchantDeclaration, src config.MerchantDeclaration) {
	if strings.TrimSpace(src.DisplayName) != "" {
		dst.DisplayName = src.DisplayName
	}
	if strings.TrimSpace(src.APIHost) != "" {
		dst.APIHost = src.APIHost
	}
	if len(src.Custodians) > 0 {
		if dst.Custodians == nil {
			dst.Custodians = map[string]config.CustodianConfig{}
		}
		for key, srcEntry := range src.Custodians {
			dstEntry := dst.Custodians[key]
			MergeCustodianConfig(&dstEntry, srcEntry)
			dst.Custodians[key] = dstEntry
		}
	}
	if len(src.PSPs) > 0 {
		if dst.PSPs == nil {
			dst.PSPs = map[string]config.PSPConfig{}
		}
		for key, srcAccount := range src.PSPs {
			dstAccount := dst.PSPs[key]
			MergePSPConfig(&dstAccount, srcAccount)
			dst.PSPs[key] = dstAccount
		}
	}
}

func MergeCustodianConfig(dst *config.CustodianConfig, src config.CustodianConfig) {
	if strings.TrimSpace(src.Kind) != "" {
		dst.Kind = src.Kind
	}
	if strings.TrimSpace(src.AccountID) != "" {
		dst.AccountID = src.AccountID
	}
	if src.Archived {
		dst.Archived = true
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

func MergePSPConfig(dst *config.PSPConfig, src config.PSPConfig) {
	if strings.TrimSpace(string(src.Rail)) != "" {
		dst.Rail = src.Rail
	}
	if strings.TrimSpace(src.AccountID) != "" {
		dst.AccountID = src.AccountID
	}
	if strings.TrimSpace(src.Custodian) != "" {
		dst.Custodian = src.Custodian
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

// MerchantManifestReconcileOptions selects the apply tier (#527). The default
// (both false) is additive + seed-once. Startup provisioning always uses the
// default; the destructive tiers are opt-in via the CLI and never run on boot.
type MerchantManifestReconcileOptions struct {
	StripeClients *stripeapi.Factory
	// Insert creates missing merchant/issuer/profile/PSP/secret
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
	// from the manifest, reconciling the secret set to the file. PSP
	// and issuer removal stay reversible/manual and are not pruned here.
	Prune bool
	// SecretStore overrides where manifest secrets reconcile to. MODE 1 (#723)
	// boot paths pass the runtime's in-memory manifest plane
	// (ManifestSecretStore.Seeder()); nil selects by mode — api mode builds the
	// persistent backend, manifest mode uses an EPHEMERAL in-memory store
	// (validation only; the long-running server seeds its own plane at boot).
	SecretStore merchants.MerchantSecretStore
	// IdentityResolver is an optional test/embedding seam for PSP
	// discovery. Production uses the default resolver over provider read-only
	// identity APIs.
	IdentityResolver ManifestProviderIdentityResolver
	// DeferPSP, when set, skips a PSP whose reconcile failed with an error it
	// accepts (a provider that cannot answer now) instead of failing the
	// whole reconcile; the caller retries it in the background.
	DeferPSP func(rail string, err error) bool
	// SolanaTransit, when set, is the serving runtime's Transit client: the
	// reconcile uses it instead of opening (and waiting on) its own Vault login.
	SolanaTransit solana.TransitClient
	// WrapTransit, when set, wraps the Transit client for one merchant's
	// provisioning (the signer identity check).
	WrapTransit func(slug string, transit solana.TransitClient) solana.TransitClient
}

type ManifestProviderIdentityResolver interface {
	ResolveManifestPSP(ctx context.Context, cfg *config.Config, rail, environment string, account config.PSPConfig, secrets ManifestSecretValues) (ManifestProviderIdentity, error)
}

type ManifestProviderIdentity struct {
	AccountID   string
	DisplayName *string
}

func (o MerchantManifestReconcileOptions) HasMutations() bool {
	return o.Insert || o.Overwrite || o.Prune
}

// ProvisionMerchant is the single OpenRails merchant-provisioning boundary
// (#527). Standalone calls it with a control plane, which creates/ensures the
// AuthKit permission-group and optional issuer-as-owner before recording
// permission_group_id. Embedded calls it with only Database, which registers an
// ownerless merchant row and applies the same profile/PSP
// configuration path without touching AuthKit or startup bootstrap markers.
type ProvisionMerchantRequest struct {
	// MerchantID is an already resolved, explicit host binding. The outer name
	// boundary must verify the supplied name before passing this immutable scope.
	MerchantID    billing.MerchantID
	Directory     *merchants.Service
	Config        *config.Config
	Database      *db.DB
	SecretStore   merchants.MerchantSecretStore
	SolanaTransit solana.TransitClient
	Slug          string
	Merchant      config.MerchantDeclaration
	Options       MerchantManifestReconcileOptions
}

func ProvisionMerchant(ctx context.Context, req ProvisionMerchantRequest) (*merchants.Merchant, error) {
	slug := billing.NormalizeMerchantSlug(req.Slug)
	mt := req.Merchant
	if err := ValidateMerchantDeclaration(req.Config, mt); err != nil {
		return nil, err
	}
	database := req.Database
	if database == nil {
		return nil, fmt.Errorf("merchant provisioning requires database")
	}

	directory := req.Directory
	if directory == nil {
		var err error
		directory, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return nil, err
		}
	}
	var tn *merchants.Merchant
	var err error
	if !req.MerchantID.IsZero() {
		tn, err = directory.Get(ctx, req.MerchantID)
		if err == nil {
			tn.Slug = slug
		}
	} else {
		tn, err = directory.GetBySlug(ctx, slug)
	}
	found := err == nil
	if errors.Is(err, merchants.ErrMerchantNotFound) && req.MerchantID.IsZero() {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if !found {
		if !req.Options.Insert {
			return nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
		}
		tn, err = provisionMerchantIdentity(ctx, database, slug, mt)
		if err != nil {
			return nil, err
		}
	}

	// Keep an existing merchant's display name in sync with the manifest (the
	// create path already set it). A UUID-scoped update ensures an
	// empty manifest display name leaves the stored one untouched.
	if found && req.Options.Overwrite && strings.TrimSpace(mt.DisplayName) != "" {
		directory, err := merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return nil, err
		}
		if err := directory.SetDisplayName(ctx, tn.ID, mt.DisplayName); err != nil {
			return nil, fmt.Errorf("merchant bootstrap: sync display name for %q: %w", slug, err)
		}
	}

	// Startup ensures identity and missing accounts. Existing metadata belongs
	// to ordinary Client operations; restarting a declaration cannot reassert it.
	if found && !req.Options.Overwrite {
		mt.DisplayName = ""
		mt.APIHost = ""
		mt.Settings = billing.MerchantSettings{}
	}
	if err := ReconcileManifestMerchantConfiguration(ctx, req.Config, database, tn.ID, slug, mt, req.SecretStore, req.SolanaTransit, req.Options); err != nil {
		return nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, nil
}

func provisionMerchantIdentity(ctx context.Context, database *db.DB, slug string, mt config.MerchantDeclaration) (*merchants.Merchant, error) {
	id, err := db.RegisterUnboundMerchant(ctx, database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: slug, DisplayName: mt.DisplayName})
	if err != nil {
		return nil, err
	}
	directory, err := merchants.NewDirectoryService(database.DataPool())
	if err != nil {
		return nil, err
	}
	return directory.Get(ctx, id)
}

func sortedMerchantKeys(in map[string]config.MerchantDeclaration) []string {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type PspEntry struct {
	key    string
	rail   string
	config config.PSPConfig
}

// PspEntries are the declared PSPs in key order.
func PspEntries(in map[string]config.PSPConfig) []PspEntry {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]PspEntry, 0, len(in))
	for _, key := range keys {
		out = append(out, PspEntry{key: key, rail: string(in[key].Rail), config: in[key]})
	}
	return out
}

type CustodianEntry struct {
	key    string
	kind   string
	config config.CustodianConfig
}

// CustodianEntries are the declared custodians in key order.
func CustodianEntries(in map[string]config.CustodianConfig) []CustodianEntry {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]CustodianEntry, 0, len(in))
	for _, key := range keys {
		out = append(out, CustodianEntry{key: key, kind: in[key].Kind, config: in[key]})
	}
	return out
}

// ResolvedManifestCustodian is the store-independent front half of a custodian
// reconcile: normalized kind, derived environment, identity and validated
// settings/secret slots. The SAME validator (config.ValidateCustodianEntry)
// runs on the store plane, so neither ingestion path can accept a declaration
// the other would reject.
type ResolvedManifestCustodian struct {
	key         string
	kind        string
	environment string
	accountID   string
	settings    map[string]any
	archived    bool
}

func ResolveManifestCustodian(cfg *config.Config, entry CustodianEntry) (ResolvedManifestCustodian, error) {
	out := ResolvedManifestCustodian{
		key:         strings.TrimSpace(entry.key),
		kind:        custodians.Normalize(entry.kind),
		environment: config.ExpectedProviderEnvironment(cfg != nil && config.IsTestMode(cfg)),
		accountID:   strings.TrimSpace(entry.config.AccountID),
		settings:    entry.config.Settings,
		archived:    entry.config.Archived,
	}
	secretKeys := make([]string, 0, len(entry.config.Secrets))
	for key := range entry.config.Secrets {
		secretKeys = append(secretKeys, key)
	}
	sort.Strings(secretKeys)
	if err := config.ValidateCustodianEntry(config.CustodianEntry{
		Key:        out.key,
		Kind:       out.kind,
		AccountID:  out.accountID,
		Settings:   out.settings,
		Archived:   out.archived,
		SecretKeys: secretKeys,
	}); err != nil {
		return ResolvedManifestCustodian{}, err
	}
	return out, nil
}

// SeedManifestCustodianSecrets writes one custodian's declared credentials
// under their identity-scoped names. Seed-once/overwrite/insert posture is the
// PSP one — a value rotated out of band is never reverted to the manifest seed.
func SeedManifestCustodianSecrets(ctx context.Context, merchantID billing.MerchantID, rc ResolvedManifestCustodian, declared map[string]string, store merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions, seedOnly bool) error {
	for key, value := range declared {
		name, err := merchants.CustodianSecretName(rc.kind, rc.environment, rc.accountID, key)
		if err != nil {
			return err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("custodian %q (%s): secret %s is empty", rc.key, rc.kind, key)
		}
		if !seedOnly {
			_, gerr := store.Get(ctx, merchantID, name)
			switch {
			case gerr == nil && !opts.Overwrite:
				continue
			case errors.Is(gerr, merchants.ErrSecretNotFound) && !opts.Insert:
				return fmt.Errorf("secret %s is missing; rerun with --insert to create it", name)
			case gerr != nil && !errors.Is(gerr, merchants.ErrSecretNotFound):
				return fmt.Errorf("check secret %s: %w", name, gerr)
			}
		}
		if _, err := store.Put(ctx, merchantID, name, value); err != nil {
			return fmt.Errorf("store secret %s: %w", name, err)
		}
	}
	return nil
}

// ReconcileManifestCustodians converges the merchant's declared custodians and
// returns them by declared key, so the PSP pass can resolve its `custodian:`
// reference to a row id. Custodians land BEFORE PSPs for the obvious reason:
// psps.custodian_id is a foreign key.
func ReconcileManifestCustodians(ctx context.Context, cfg *config.Config, database *db.DB, merchantID billing.MerchantID, mt config.MerchantDeclaration, secretStore merchants.MerchantSecretStore, opts MerchantManifestReconcileOptions) (map[string]gen.BillingCustodian, error) {
	out := map[string]gen.BillingCustodian{}
	entries := CustodianEntries(mt.Custodians)
	if len(entries) == 0 {
		return out, nil
	}
	if secretStore == nil {
		return nil, fmt.Errorf("merchant bootstrap: custodian secrets require a secret store")
	}
	seen := map[string]string{}
	for _, entry := range entries {
		rc, err := ResolveManifestCustodian(cfg, entry)
		if err != nil {
			return nil, err
		}
		lower := strings.ToLower(rc.key)
		if prior, dup := seen[lower]; dup {
			return nil, fmt.Errorf("custodian key %q is declared twice (%s and %s) — a PSP reference must resolve to one custodian", rc.key, prior, rc.kind)
		}
		seen[lower] = rc.kind
		if err := SeedManifestCustodianSecrets(ctx, merchantID, rc, entry.config.Secrets, secretStore, opts, false); err != nil {
			return nil, err
		}
		settingsJSON, err := json.Marshal(NonNilSettings(rc.settings))
		if err != nil {
			return nil, fmt.Errorf("encode custodian settings: %w", err)
		}
		archived := rc.archived
		environment := rc.environment
		// #650: a custodian identity belongs to exactly one merchant. Say so
		// clearly, rather than letting the global-uniqueness upsert reject it
		// with an opaque unique violation.
		if err := merchants.AssertCustodianUnowned(ctx, gen.New(database.DataPool()), merchantID.UUID(), rc.kind, rc.environment, rc.accountID); err != nil {
			return nil, err
		}
		// Same apply tiers as a PSP (#527): plan-only runs mutate nothing, and
		// without --overwrite an existing declaration is left as it stands.
		mctx := merchant.WithID(ctx, merchantID)
		var row gen.BillingCustodian
		found := true
		if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
			var err error
			row, err = database.Gen(ctx).GetCustodianByIdentity(ctx, gen.GetCustodianByIdentityParams{
				MerchantID:  merchantID.UUID(),
				Kind:        rc.kind,
				Environment: &environment,
				AccountID:   rc.accountID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				found = false
				return nil
			}
			return err
		}); err != nil {
			return nil, fmt.Errorf("lookup custodian %q: %w", rc.key, err)
		}
		if !found && !opts.Insert {
			return nil, fmt.Errorf("custodian %s:%s:%s is missing; rerun with --insert to create it", rc.kind, environment, rc.accountID)
		}
		if found && !opts.Overwrite {
			out[lower] = row
			continue
		}
		if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
			var err error
			if !opts.Overwrite {
				if err := database.Gen(ctx).InsertSnapshotCustodian(ctx, gen.InsertSnapshotCustodianParams{MerchantID: merchantID.UUID(), Key: rc.key, Kind: rc.kind, Environment: environment, AccountID: rc.accountID, Settings: settingsJSON, Archived: archived}); err != nil {
					return err
				}
				row, err = database.Gen(ctx).GetCustodianByIdentity(ctx, gen.GetCustodianByIdentityParams{MerchantID: merchantID.UUID(), Kind: rc.kind, Environment: &environment, AccountID: rc.accountID})
				return err
			}
			row, err = database.Gen(ctx).UpsertCustodian(ctx, gen.UpsertCustodianParams{
				MerchantID:  merchantID.UUID(),
				Key:         rc.key,
				Kind:        rc.kind,
				Environment: &environment,
				AccountID:   rc.accountID,
				Settings:    settingsJSON,
				Archived:    &archived,
				// or#812: the manifest SEEDS credentials, it does not rotate
				// them, so it records no floor — and an empty map leaves any
				// floor an API rotation recorded exactly where it was.
				CredentialVersions: nil,
			})
			return err
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("custodian %q (%s): tenant %s: %w", rc.key, rc.kind, rc.accountID, merchants.ErrCustodianOwnedByAnotherMerchant)
			}
			return nil, fmt.Errorf("upsert custodian %q: %w", rc.key, err)
		}
		out[lower] = row
	}
	return out, nil
}

func NonNilSettings(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

// ResolveManifestCustodianReference resolves a PSP's `custodian:` key against
// the merchant's declared custodians. An undeclared key is a HARD error: a PSP
// that means to charge a vault-held card and cannot find the vault must not
// arm as though its gateway held the card.
func ResolveManifestCustodianReference(rail string, account config.PSPConfig, declared map[string]gen.BillingCustodian) (*uuid.UUID, error) {
	key := strings.ToLower(strings.TrimSpace(account.Custodian))
	if key == "" {
		return nil, nil
	}
	row, ok := declared[key]
	if !ok {
		known := make([]string, 0, len(declared))
		for k := range declared {
			known = append(known, k)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return nil, fmt.Errorf("psp on rail %q references custodian %q, but this merchant declares no custodians:", rail, account.Custodian)
		}
		return nil, fmt.Errorf("psp on rail %q references custodian %q, which is not declared (declared: %s)", rail, account.Custodian, strings.Join(known, ", "))
	}
	d, err := custodians.Require(row.Kind)
	if err != nil {
		return nil, err
	}
	if !d.SupportsRail(models.Rail(NormalizeManifestRail(rail))) {
		return nil, fmt.Errorf("psp on rail %q references custodian %q (%s), which can only be charged through %s — that rail has the detokenizing proxy path", rail, account.Custodian, d.Kind, d.RailNames())
	}
	id := row.ID
	return &id, nil
}

func ReconcileManifestMerchantConfiguration(ctx context.Context, cfg *config.Config, database *db.DB, merchantID billing.MerchantID, slug string, mt config.MerchantDeclaration, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
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
	// The declared settings go through the configuration application's merge
	// and validation: one path for mode 1 and mode 2.
	settings := mt.Settings
	if settings.Profile != nil && strings.TrimSpace(settings.Profile.DisplayName) == "" {
		profile := *settings.Profile
		profile.DisplayName = strings.TrimSpace(mt.DisplayName)
		settings.Profile = &profile
	}
	if err := service.ApplyDeclaredMerchantSettings(mctx, database, settings); err != nil {
		return fmt.Errorf("merchant settings: %w", err)
	}

	// or#880: custodians land FIRST — psps.custodian_id is a foreign key, and
	// a PSP that names an undeclared custodian must fail loudly here rather
	// than arm as though its gateway held the card.
	declaredCustodians, err := ReconcileManifestCustodians(ctx, cfg, database, merchantID, mt, secretStore, opts)
	if err != nil {
		return err
	}

	for _, entry := range PspEntries(mt.PSPs) {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: PSP secrets require a secret store")
		}
		custodianID, err := ResolveManifestCustodianReference(entry.rail, entry.config, declaredCustodians)
		if err != nil {
			return err
		}
		if err := ReconcileManifestPSP(ctx, cfg, database, merchantID, slug, entry.key, entry.rail, entry.config, custodianID, secretStore, transit, opts); err != nil {
			if opts.DeferPSP != nil && opts.DeferPSP(entry.rail, err) {
				log.WithError(err).WithField("psp", entry.key).Warn("merchant bootstrap: PSP deferred until its provider answers")
				continue
			}
			return err
		}
	}
	if opts.Prune {
		if secretStore == nil {
			return fmt.Errorf("merchant bootstrap: prune requires a secret store")
		}
		if err := PruneManifestSecrets(ctx, cfg, merchantID, mt, secretStore); err != nil {
			return err
		}
	}
	return nil
}

// PruneManifestSecrets deletes secrets held for the merchant that the manifest
// no longer declares (#527 --prune), reconciling the stored secret set to the
// file. Names are derived exactly as Put derives them.
func PruneManifestSecrets(ctx context.Context, cfg *config.Config, merchantID billing.MerchantID, mt config.MerchantDeclaration, secretStore merchants.MerchantSecretStore) error {
	declared := map[string]struct{}{}
	for _, entry := range CustodianEntries(mt.Custodians) {
		rc, err := ResolveManifestCustodian(cfg, entry)
		if err != nil {
			return err
		}
		for key := range entry.config.Secrets {
			name, err := merchants.CustodianSecretName(rc.kind, rc.environment, rc.accountID, key)
			if err != nil {
				return err
			}
			declared[name] = struct{}{}
		}
	}
	for _, entry := range PspEntries(mt.PSPs) {
		rail := NormalizeManifestRail(entry.rail)
		environment := ManifestProviderEnvironment(cfg)
		accountID := strings.TrimSpace(entry.config.AccountID)
		if accountID == "" {
			if rail != string(models.RailSolana) {
				return fmt.Errorf("PSP %q account_id is required before pruning secrets", rail)
			}
			if len(entry.config.Secrets) == 0 {
				continue
			}
			secrets, err := NewManifestSecretValues(rail, entry.config.Secrets)
			if err != nil {
				return err
			}
			if _, ok := secrets.sources["private_key"]; !ok {
				return fmt.Errorf("PSP %q private_key is required before pruning secrets without account_id", rail)
			}
			accountID, err = SolanaLocalKeypairPublicKey(secrets)
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

// ResolvedManifestRailAccount is the store/DB-independent front half of a
// manifest rail-account reconcile: normalized rail, resolved environment,
// account identity and secret values, fully validated. Shared by the DB
// reconcile path and the seeding-only plane build (#723).
type ResolvedManifestRailAccount struct {
	rail           string
	environment    string
	accountID      string
	secrets        ManifestSecretValues
	identity       ManifestProviderIdentity
	signerEvidence map[string]string
}

func ResolveManifestRailAccount(ctx context.Context, cfg *config.Config, rail string, account config.PSPConfig, transit solana.TransitClient, resolver ManifestProviderIdentityResolver) (ResolvedManifestRailAccount, error) {
	out := ResolvedManifestRailAccount{rail: NormalizeManifestRail(rail)}
	if out.rail == "" {
		return out, fmt.Errorf("PSP rail is required")
	}
	environment := ManifestProviderEnvironment(cfg)
	out.environment = environment
	// #711: the Solana runtime knobs live in the account settings block —
	// validate strictly at push time so a typo'd key/value fails loudly here
	// instead of being stored inert.
	if out.rail == string(models.RailSolana) {
		if err := config.ValidateSolanaAccountSettings(account.Settings); err != nil {
			return out, fmt.Errorf("PSP %q: %w", out.rail, err)
		}
		// or#881: the token declaration is resolved against the built-in mint
		// registry for THIS account's network, so a restated built-in mint or a
		// custom token with no mint fails the push instead of at arm time.
		settings, err := config.ParseSolanaAccountSettings(account.Settings)
		if err != nil {
			return out, fmt.Errorf("PSP %q: %w", out.rail, err)
		}
		if _, err := solanatokens.ResolveDeclared(ManifestSolanaNetwork(environment), settings.Tokens); err != nil {
			return out, fmt.Errorf("PSP %q: %w", out.rail, err)
		}
	}
	// or#880: custody has its own declaration now (`custodians:` + the PSP's
	// `custodian:` reference). An inline custody block in a PSP's settings is
	// a retired shape and fails the push loudly rather than being stored inert
	// on a money path — the reference itself is checked by the caller, which
	// is the pass that holds the declared custodians.
	if err := config.RejectRetiredCustodySettings(account.Settings); err != nil {
		return out, fmt.Errorf("PSP %q: %w", out.rail, err)
	}
	// #1129: server card entry is refused on a rail with no server-side vault
	// call, and beside a custodian.
	if _, err := config.CardEntry(out.rail, account.Settings, strings.TrimSpace(account.Custodian) != ""); err != nil {
		return out, fmt.Errorf("PSP %q: %w", out.rail, err)
	}
	secrets, err := NewManifestSecretValues(out.rail, account.Secrets)
	if err != nil {
		return out, err
	}
	out.secrets = secrets
	if resolver == nil {
		resolver = DefaultManifestProviderIdentityResolver{}
	}
	identity, err := resolver.ResolveManifestPSP(ctx, cfg, out.rail, environment, account, secrets)
	if err != nil {
		return out, err
	}
	out.identity = identity
	accountID := strings.TrimSpace(identity.AccountID)
	// For Solana, ManifestProviderSignerEvidence derives the stored PSP
	// identity from the signer key; a declared account_id is ignored (warned).
	signerEvidence, accountID, err := ManifestProviderSignerEvidence(ctx, out.rail, accountID, account, secrets, transit)
	if err != nil {
		return out, err
	}
	out.signerEvidence = signerEvidence
	if accountID == "" {
		if out.rail == string(models.RailSolana) {
			return out, fmt.Errorf("PSP %q requires signer-derived identity", out.rail)
		}
		return out, fmt.Errorf("PSP %q requires account_id", out.rail)
	}
	// #697: rail-specific format doctrine (CCBill ids are dash-joined).
	if err := config.ValidateRailAccountID(models.Rail(out.rail), accountID); err != nil {
		return out, fmt.Errorf("PSP %q: %w", out.rail, err)
	}
	out.accountID = accountID
	return out, nil
}

// SeedMerchantManifestSecretPlane resolves ONE merchant's manifest-declared
// rail secrets and seeds them into store, touching no DB state — the same
// values the server's boot reconcile seeds into its runtime plane. MODE-1
// one-off processes (pull-provider CLI, #723) build their ephemeral in-memory
// plane through it and arm per-merchant fetchers from the on-disk manifest.
func SeedMerchantManifestSecretPlane(ctx context.Context, cfg *config.Config, merchantID billing.MerchantID, mt config.MerchantDeclaration, store merchants.MerchantSecretStore, transit solana.TransitClient) error {
	if store == nil {
		return fmt.Errorf("merchant manifest secret plane: store is required")
	}
	for _, entry := range CustodianEntries(mt.Custodians) {
		rc, err := ResolveManifestCustodian(cfg, entry)
		if err != nil {
			return err
		}
		if err := SeedManifestCustodianSecrets(ctx, merchantID, rc, entry.config.Secrets, store, MerchantManifestReconcileOptions{}, true); err != nil {
			return err
		}
	}
	for _, entry := range PspEntries(mt.PSPs) {
		ra, err := ResolveManifestRailAccount(ctx, cfg, entry.rail, entry.config, transit, nil)
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

func ReconcileManifestPSP(ctx context.Context, cfg *config.Config, database *db.DB, merchantID billing.MerchantID, merchantSlug, localKey, rail string, account config.PSPConfig, custodianID *uuid.UUID, secretStore merchants.MerchantSecretStore, transit solana.TransitClient, opts MerchantManifestReconcileOptions) error {
	ra, err := ResolveManifestRailAccount(ctx, cfg, rail, account, transit, opts.IdentityResolver)
	if err != nil {
		return err
	}
	if config.SecretStoreBackend(cfg) != config.SecretBackendSnapshot {
		return fmt.Errorf("managed provider declarations require the Client payment-provider publication operation with operation ID and expected revision")
	}
	// Bind credential custody before loading any replacement snapshot material.
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		row, err := database.Gen(ctx).GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{MerchantID: merchantID.UUID(), Rail: ra.rail, Environment: &ra.environment, AccountID: ra.accountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		custody := ""
		if row.CredentialCustody != nil {
			custody = *row.CredentialCustody
		}
		if custody != "" && custody != "snapshot" {
			return fmt.Errorf("provider credential custody differs from the selected snapshot; explicit custody migration is required")
		}
		if custody == "" && len(row.CredentialVersions) > 0 && string(row.CredentialVersions) != "{}" {
			return fmt.Errorf("published managed credentials cannot be replaced by a startup snapshot")
		}
		return nil
	}); err != nil {
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
	found := false
	if err := database.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		_, err := database.Gen(ctx).GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{
			MerchantID:  merchantID.UUID(),
			Rail:        rail,
			Environment: StringPtrIfNotEmpty(environment),
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
		return fmt.Errorf("lookup PSP %s:%s:%s: %w", rail, environment, accountID, err)
	}
	if !found && !opts.Insert {
		return fmt.Errorf("PSP %s:%s:%s is missing; rerun with --insert to create it", rail, environment, accountID)
	}
	if found && !opts.Overwrite {
		return nil
	}
	key := ""
	if identity.DisplayName != nil {
		key = strings.TrimSpace(*identity.DisplayName)
	}
	if n := strings.TrimSpace(localKey); n != "" {
		key = n
	}
	key = strings.ToLower(key)
	if key == "" {
		return fmt.Errorf("PSP %s:%s needs a key", rail, accountID)
	}
	settings := account.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	settingsJSON, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode PSP settings: %w", err)
	}
	var signerJSON []byte
	if signerEvidence != nil {
		if signerJSON, err = json.Marshal(signerEvidence); err != nil {
			return fmt.Errorf("encode PSP signer: %w", err)
		}
	}
	// #650: a PSP belongs to exactly one merchant. Fail with a clear
	// error if another merchant already owns this identity, rather than letting the
	// global-uniqueness upsert reject it with an opaque unique-violation.
	if err := merchants.AssertPSPUnowned(ctx, gen.New(database.DataPool()), merchantID.UUID(), rail, environment, accountID); err != nil {
		return err
	}
	mctx := merchant.WithID(ctx, merchantID)
	if err := database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		// #662: derive the id from the global natural key and store the SAME
		// normalized (rail, environment, account_id) it is hashed from.
		railAcctID, nRail, nEnv, nAccount := merchants.PSPNaturalKey(rail, environment, accountID)
		if !opts.Overwrite {
			if err := database.Gen(ctx).InsertSnapshotPSP(ctx, gen.InsertSnapshotPSPParams{ID: railAcctID, MerchantID: merchantID.UUID(), Rail: nRail, Environment: nEnv, AccountID: nAccount, Key: key, Archived: account.Archived, Settings: settingsJSON, Signer: signerJSON, CustodianID: custodianID}); err != nil {
				return err
			}
			_, err := database.Gen(ctx).GetPSPByIdentity(ctx, gen.GetPSPByIdentityParams{MerchantID: merchantID.UUID(), Rail: nRail, Environment: &nEnv, AccountID: nAccount})
			return err
		}
		_, err := database.Gen(ctx).UpsertManifestPSP(ctx, gen.UpsertManifestPSPParams{
			ID:          railAcctID,
			MerchantID:  merchantID.UUID(),
			Key:         key,
			Rail:        nRail,
			Environment: nEnv,
			AccountID:   nAccount,
			Archived:    account.Archived,
			CustodianID: custodianID,
			Settings:    settingsJSON,
			Signer:      signerJSON,
		})
		if err != nil {
			return fmt.Errorf("upsert PSP %s:%s: %w", rail, accountID, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// ManifestProviderSignerEvidence validates the Solana signer and returns signer
// evidence plus the derived PSP identity. Solana never needs
// account_id — the stored DB identity is always the signer public key; a declared
// value is ignored (warned).
func ManifestProviderSignerEvidence(ctx context.Context, rail, accountID string, account config.PSPConfig, secrets ManifestSecretValues, transit solana.TransitClient) (map[string]string, string, error) {
	if rail == string(models.RailSolana) && strings.TrimSpace(accountID) != "" {
		log.Warnf("solana PSP: declared account_id %s is ignored; it is always derived from the signer's public key", strings.TrimSpace(accountID))
		accountID = ""
	}
	if account.Signer == nil {
		if _, ok := secrets.sources["private_key"]; ok && rail == string(models.RailSolana) {
			pub, err := SolanaLocalKeypairPublicKey(secrets)
			if err != nil {
				return nil, "", err
			}
			return map[string]string{"mode": "local_keypair"}, pub, nil
		}
		return nil, accountID, nil
	}
	if rail != string(models.RailSolana) {
		return nil, "", fmt.Errorf("PSP signer is only supported for solana")
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
		pub, err := SolanaLocalKeypairPublicKey(secrets)
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
		pub, err := SolanaTransitPublicKey(ctx, transit, key)
		if err != nil {
			return nil, "", err
		}
		return map[string]string{"mode": "vault_transit", "key": key}, pub, nil
	default:
		return nil, "", fmt.Errorf("solana signer mode must be local_keypair or vault_transit")
	}
}

// SolanaLocalKeypairPublicKey parses the base58 private_key secret and returns its
// Solana address (base58 public key).
func SolanaLocalKeypairPublicKey(secrets ManifestSecretValues) (string, error) {
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

// SolanaTransitPublicKey reads the Vault Transit Ed25519 key's public key and
// returns its Solana address (base58). The private key never leaves Vault.
func SolanaTransitPublicKey(ctx context.Context, transit solana.TransitClient, key string) (string, error) {
	raw, err := transit.PublicKey(ctx, key)
	if err != nil {
		return "", fmt.Errorf("solana vault transit signer %q public key: %w", key, err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("solana vault transit signer %q public key is %d bytes, want 32", key, len(raw))
	}
	return solanago.PublicKeyFromBytes(raw).String(), nil
}

// ManifestProviderEnvironment derives a PSP's environment from deployment
// posture: test under test_mode, live otherwise. It is never declared.
func ManifestProviderEnvironment(cfg *config.Config) string {
	return config.ExpectedProviderEnvironment(cfg != nil && config.IsTestMode(cfg))
}

// ManifestSolanaNetwork maps a PSP environment onto the Solana network the same
// way railresolve does at arm time (#349: network derives from test_mode alone).
func ManifestSolanaNetwork(environment string) string {
	if environment == config.ProviderEnvironmentTest {
		return "devnet"
	}
	return "mainnet"
}

func NormalizeManifestRail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

type ManifestSecretValues struct {
	rail    string
	sources map[string]string
	values  map[string]string
}

func NewManifestSecretValues(rail string, sources map[string]string) (ManifestSecretValues, error) {
	out := ManifestSecretValues{
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
			return out, fmt.Errorf("duplicate PSP secret key %q", canonical)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			return out, fmt.Errorf("PSP secret %s.%s is empty", rail, canonical)
		}
		out.sources[canonical] = value
	}
	return out, nil
}

func (v ManifestSecretValues) Resolve(key string, fallback string) (string, error) {
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
		return "", fmt.Errorf("PSP secret %s.%s is empty", v.rail, canonical)
	}
	v.values[canonical] = value
	return value, nil
}

func (v ManifestSecretValues) ResolveIfPresent(key string) (string, bool, error) {
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

type DefaultManifestProviderIdentityResolver struct{}

func (DefaultManifestProviderIdentityResolver) ResolveManifestPSP(ctx context.Context, cfg *config.Config, rail, environment string, account config.PSPConfig, secrets ManifestSecretValues) (ManifestProviderIdentity, error) {
	if accountID := strings.TrimSpace(account.AccountID); accountID != "" {
		return ManifestProviderIdentity{AccountID: accountID}, nil
	}
	if rail == string(models.RailSolana) {
		return ManifestProviderIdentity{}, nil
	}
	// Auto-discovery via live credentials was removed (#592): every rail must
	// declare account_id in the manifest.
	return ManifestProviderIdentity{}, fmt.Errorf("provider account_id is required for %s (auto-discovery removed; declare account_id in the manifest)", rail)
}

func StringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func ValidateMerchantDeclaration(cfg *config.Config, mt config.MerchantDeclaration) error {
	// Validate declarations before identity creation or seed-once suppression.
	// An existing merchant must not turn malformed input into a successful boot.
	if err := service.ValidateMerchantSettings(mt.Settings); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if host := merchants.NormalizeAPIHost(mt.APIHost); host != "" {
		if err := merchants.ValidateAPIHost(host); err != nil {
			return err
		}
	}
	for key, account := range mt.PSPs {
		rail := NormalizeManifestRail(string(account.Rail))
		// A CCBill account with inline credentials must sign its FlexForm links.
		if rail == string(models.RailCCBill) && len(account.Secrets) > 0 && strings.TrimSpace(account.Secrets["salt"]) == "" {
			return fmt.Errorf("psps.%s.secrets.salt is required", key)
		}
		if rail != string(models.RailStripe) {
			continue
		}
		if raw, ok := account.Settings["publishable_key"]; ok {
			value, _ := raw.(string)
			if err := config.ValidateStripePublishableKeyPosture(cfg, value); err != nil {
				return fmt.Errorf("psps.%s.settings.publishable_key: %w", key, err)
			}
		}
	}
	if config.SecretStoreBackend(cfg) != config.SecretBackendSnapshot && (len(mt.PSPs) > 0 || len(mt.Custodians) > 0) {
		return fmt.Errorf("managed provider declarations require explicit Client publication operations; startup metadata and credential custody are separate")
	}
	return nil
}
