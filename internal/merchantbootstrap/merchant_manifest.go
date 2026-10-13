package merchantbootstrap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"sort"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/goccy/go-yaml"
	koanfyaml "github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/signeridentity"
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

// ValidateMerchantSecretOverlay limits operator-mounted overlays to secret
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
			return fmt.Errorf("merchant secret overlay only accepts merchants.<slug>.psps.<psp>.secrets and merchants.<slug>.secrets.scim_token (found %q)", key)
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
			if key != "psps" && key != "secrets" {
				return fmt.Errorf("merchant secret overlay cannot set merchants.%s.%s; structural manifest fields stay in the base manifest", slug, key)
			}
		}
		if raw, ok := merchant["secrets"]; ok {
			secrets, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("merchant secret overlay merchants.%s.secrets must be a mapping", slug)
			}
			for key := range secrets {
				if key != "scim_token" {
					return fmt.Errorf("merchant secret overlay cannot set merchants.%s.secrets.%s", slug, key)
				}
			}
		}
		if _, ok := merchant["psps"]; !ok {
			continue
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
	if err := RejectMisplacedMerchantConfigKeys(raw); err != nil {
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

// RejectMisplacedMerchantConfigKeys fails a manifest entry that sets a
// retired key (config.RetiredDeclarationKey), or that names its own slug:
// the entry's key is the slug, so the two can never disagree. The strict
// parser would reject these as unknown fields anyway, but "unknown field"
// reads like a typo: each deserves a pointer.
func RejectMisplacedMerchantConfigKeys(raw []byte) error {
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
		if _, ok := probe.Merchants[slug]["slug"]; ok {
			return fmt.Errorf("merchants.%s.slug is not accepted: the entry's key is its slug", slug)
		}
		if retired, ok := config.RetiredDeclarationKey(probe.Merchants[slug]); ok {
			return fmt.Errorf("merchants.%s.%s", slug, retired)
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
	if strings.TrimSpace(src.Secrets.SCIMToken) != "" {
		dst.Secrets.SCIMToken = src.Secrets.SCIMToken
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

// ProvisionMerchantParams provisions one declared merchant (#527). Standalone
// calls it with the control plane's merchant already registered; embedded
// calls it with Insert, registering an ownerless merchant row.
type ProvisionMerchantParams struct {
	// MerchantID is an already resolved, explicit host binding.
	MerchantID billing.MerchantID
	Config     *config.Config
	Database   *db.DB
	// Merchants is the merchants service over the configuration cache the
	// declaration fills (a file) or seeds (Vault).
	Merchants *merchants.Service
	Slug      string
	Merchant  config.MerchantDeclaration
	// Insert registers the merchant when it is missing.
	Insert bool
	// SolanaTransit reads a vault_transit Solana signer's public key.
	SolanaTransit solana.TransitClient
	// DeferPSP, when set, skips a PSP whose account cannot be derived now (a
	// Transit signer Vault cannot answer) instead of failing; the caller
	// provisions again later.
	DeferPSP func(rail string, err error) bool
}

// ProvisionMerchant registers a declared merchant and, with a file as the
// source, puts its declaration in place: the declaration IS the
// configuration. A document it cannot serve is refused alone and named in
// refused, with why; the merchant serves the rest. With Vault as the source a
// declaration names only the merchant (its slug); Vault alone holds its
// configuration.
func ProvisionMerchant(ctx context.Context, req ProvisionMerchantParams) (_ *merchants.Merchant, refused map[string]string, _ error) {
	slug := billing.NormalizeMerchantSlug(req.Slug)
	mt := req.Merchant
	if err := config.RefuseDeclarationBesideVault(req.Config, slug, mt); err != nil {
		return nil, nil, err
	}
	if req.Database == nil || req.Merchants == nil || req.Merchants.Config() == nil {
		return nil, nil, fmt.Errorf("merchant provisioning requires the database and the merchant configuration")
	}
	directory := req.Merchants
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
	switch {
	case errors.Is(err, merchants.ErrMerchantNotFound) && req.MerchantID.IsZero() && req.Insert:
		id, err := db.RegisterUnboundMerchant(ctx, req.Database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: slug})
		if err != nil {
			return nil, nil, err
		}
		if tn, err = directory.Get(ctx, id); err != nil {
			return nil, nil, err
		}
	case errors.Is(err, merchants.ErrMerchantNotFound) && req.MerchantID.IsZero():
		return nil, nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
	case err != nil:
		return nil, nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if refused, err = PutDeclaration(ctx, req, tn.ID, mt); err != nil {
		return nil, nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, refused, nil
}

// PutDeclaration puts a merchant's declared configuration in place when a
// file is the source and returns each document the merchant does not serve,
// with why: what its own checks refuse (ScopeDeclaration), a Solana PSP whose
// signer cannot be read, and what the configuration's reconciliation
// rejects. One refused document never refuses the others. With Vault as the
// source there is nothing to put: a declaration naming configuration was
// refused before.
func PutDeclaration(ctx context.Context, req ProvisionMerchantParams, id billing.MerchantID, mt config.MerchantDeclaration) (map[string]string, error) {
	cache := req.Merchants.Config()
	source, ok := cache.Source().(*merchantdocs.FileSource)
	if !ok {
		return nil, config.RefuseDeclarationBesideVault(req.Config, mt.Slug, mt)
	}
	mt, refused := ScopeDeclaration(req.Config, mt)
	if signer, ok := req.SolanaTransit.(*signeridentity.Transit); ok {
		signer.Declare(mt.PSPs)
	}
	accounts := map[string]string{}
	for _, entry := range PspEntries(mt.PSPs) {
		if entry.rail != string(models.RailSolana) {
			continue
		}
		account, err := SolanaAccountID(ctx, entry.config, req.SolanaTransit)
		if err != nil {
			delete(mt.PSPs, entry.key)
			if req.DeferPSP != nil && req.DeferPSP(entry.rail, err) {
				log.WithError(err).WithField("psp", entry.key).Warn("merchant bootstrap: PSP deferred until its signer answers")
				refused[merchantdocs.PSPDoc(strings.ToLower(entry.key))] = "deferred until its signer answers: " + err.Error()
				continue
			}
			refused[merchantdocs.PSPDoc(strings.ToLower(entry.key))] = err.Error()
			continue
		}
		accounts[strings.ToLower(entry.key)] = account
	}
	set := merchantdocs.Declared(id, mt, accounts, time.Now().UTC())
	if _, ok := refused[merchantdocs.MerchantDoc]; ok {
		set.Merchant, set.HasMerchant = merchantdocs.Doc[merchantdocs.Merchant]{}, false
	}
	if len(refused) > 0 {
		set.Rejected = refused
	}
	source.Put(id, set)
	set, err := cache.Reload(ctx, id)
	if err != nil {
		return nil, err
	}
	return set.Rejected, nil
}

// ScopeDeclaration is mt without what it cannot serve, and each document it
// refused, with why: a PSP whose declaration does not check refuses that PSP
// alone. Settings that do not normalize, or a card custody the deployment
// cannot serve, refuse the merchant document and with it every PSP and
// custodian, which never serve without their merchant's settings.
func ScopeDeclaration(cfg *config.Config, mt config.MerchantDeclaration) (config.MerchantDeclaration, map[string]string) {
	refused := map[string]string{}
	mt.PSPs, mt.Custodians = maps.Clone(mt.PSPs), maps.Clone(mt.Custodians)
	if err := validateMerchantDocument(cfg, mt); err != nil {
		refused[merchantdocs.MerchantDoc] = err.Error()
		for key := range mt.PSPs {
			refused[merchantdocs.PSPDoc(strings.ToLower(strings.TrimSpace(key)))] = "the merchant document is not served"
		}
		for key := range mt.Custodians {
			refused[merchantdocs.CustodianDoc(strings.ToLower(strings.TrimSpace(key)))] = "the merchant document is not served"
		}
		mt.PSPs, mt.Custodians = nil, nil
		return mt, refused
	}
	for key, account := range mt.PSPs {
		if err := validatePSPDeclaration(key, account); err != nil {
			refused[merchantdocs.PSPDoc(strings.ToLower(strings.TrimSpace(key)))] = err.Error()
			delete(mt.PSPs, key)
		}
	}
	return mt, refused
}

// Rejected is the error an operator command answers for documents a
// declaration does not serve.
func Rejected(why map[string]string) error { return rejected(why) }

func rejected(why map[string]string) error {
	paths := make([]string, 0, len(why))
	for path := range why {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, path+": "+why[path])
	}
	return fmt.Errorf("declared configuration is not served: %s", strings.Join(parts, "; "))
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
	out := make([]PspEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, PspEntry{key: key, rail: NormalizeManifestRail(string(in[key].Rail)), config: in[key]})
	}
	return out
}

// SolanaAccountID derives a Solana PSP's account id, its signer's public key:
// a local keypair's, or the Vault Transit key's. A declared account_id is
// ignored.
func SolanaAccountID(ctx context.Context, account config.PSPConfig, transit solana.TransitClient) (string, error) {
	if err := config.ValidateSolanaAccountSettings(account.Settings); err != nil {
		return "", err
	}
	privateKey := ""
	for key, value := range account.Secrets {
		if strings.EqualFold(strings.TrimSpace(key), "private_key") {
			privateKey = strings.TrimSpace(value)
		}
	}
	mode := ""
	if account.Signer != nil {
		mode = strings.ToLower(strings.TrimSpace(account.Signer.Mode))
	}
	switch {
	case mode == "vault_transit":
		if privateKey != "" {
			return "", fmt.Errorf("solana signer mode vault_transit cannot also set secrets.private_key")
		}
		key := strings.TrimSpace(account.Signer.Key)
		if key == "" {
			return "", fmt.Errorf("solana signer mode vault_transit requires key")
		}
		if transit == nil {
			return "", fmt.Errorf("solana signer mode vault_transit requires a Vault connection")
		}
		return SolanaTransitPublicKey(ctx, transit, key)
	case mode == "" || mode == "local_keypair":
		if account.Signer != nil && strings.TrimSpace(account.Signer.Key) != "" {
			return "", fmt.Errorf("solana signer mode local_keypair must not set key")
		}
		if privateKey == "" {
			return "", fmt.Errorf("solana signer mode local_keypair requires secrets.private_key")
		}
		return SolanaLocalKeypairPublicKey(privateKey)
	default:
		return "", fmt.Errorf("solana signer mode must be local_keypair or vault_transit")
	}
}

// SolanaLocalKeypairPublicKey parses the base58 private_key secret and returns its
// Solana address (base58 public key).
func SolanaLocalKeypairPublicKey(raw string) (string, error) {
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

func NormalizeManifestRail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func StringPtrIfNotEmpty(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// ValidateMerchantDeclaration refuses a declaration any of whose documents
// ScopeDeclaration would refuse: an operator command applies all of it or
// nothing.
func ValidateMerchantDeclaration(cfg *config.Config, mt config.MerchantDeclaration) error {
	if _, refused := ScopeDeclaration(cfg, mt); len(refused) > 0 {
		if why, ok := refused[merchantdocs.MerchantDoc]; ok {
			return errors.New(why)
		}
		return rejected(refused)
	}
	return nil
}

// validateMerchantDocument checks the merchant document a declaration names
// before it is served.
func validateMerchantDocument(cfg *config.Config, mt config.MerchantDeclaration) error {
	if _, err := merchantconfig.Normalize(mt.DisplayName, mt.Settings); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	declared := merchantdocs.Declared(billing.MerchantID{}, mt, nil, time.Time{})
	var vault *config.PlatformVaultConfig
	if cfg != nil {
		vault = cfg.PlatformVault
	}
	return merchants.CardCustodyRefusal(vault, mt.Settings, declared.PSPs)
}

// validatePSPDeclaration checks one declared PSP: its rail, environment and
// keys, a CCBill account's FlexForm salt and a Stripe publishable key's
// environment.
func validatePSPDeclaration(key string, account config.PSPConfig) error {
	if err := config.ValidatePSPDeclaration(key, account); err != nil {
		return err
	}
	rail := NormalizeManifestRail(string(account.Rail))
	// A CCBill account with inline credentials must sign its FlexForm links.
	if rail == string(models.RailCCBill) && len(account.Secrets) > 0 && strings.TrimSpace(account.Secrets["salt"]) == "" {
		return fmt.Errorf("psps.%s.secrets.salt is required", key)
	}
	if rail == string(models.RailStripe) {
		if raw, ok := account.Settings["publishable_key"]; ok {
			value, _ := raw.(string)
			if err := config.ValidateStripePublishableKeyEnvironment(account.Environment, value); err != nil {
				return fmt.Errorf("psps.%s.settings.publishable_key: %w", key, err)
			}
		}
	}
	return nil
}
