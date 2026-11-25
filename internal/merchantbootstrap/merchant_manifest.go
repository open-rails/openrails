package merchantbootstrap

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/messages"
)

const DefaultMerchantConfigManifestPath = "/etc/openrails/merchants.yaml"

// Manifest is a merchant config manifest. M is its entry: the engine's
// declaration, or one embedding it with keys only its reader takes.
type Manifest[M any] struct {
	Version   int          `yaml:"version"`
	Merchants map[string]M `yaml:"merchants"`
}

// BillingConfig is the manifest as the engine reads it.
type BillingConfig = Manifest[billing.MerchantDocument]

// LoadManifestFile loads the merchant config manifest at path (LoadManifest).
func LoadManifestFile[M any](path string, check func(slug string, entry M) error) (*Manifest[M], error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("read merchant config manifest: no path given")
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-supplied manifest path
	if err != nil {
		return nil, fmt.Errorf("read merchant config manifest %s: %w", path, err)
	}
	return LoadManifest(raw, check)
}

// LoadManifest strict-parses a merchant config manifest; check vets each
// entry under its normalized slug. The engine reads no env or files.
func LoadManifest[M any](raw []byte, check func(slug string, entry M) error) (*Manifest[M], error) {
	var root map[string]any
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("load merchant config manifest: %w", err)
	}
	for _, key := range []string{"auth", "users", "groups", "roles", "permissions", "catalogs", "products"} {
		if _, ok := root[key]; ok {
			return nil, fmt.Errorf("merchant config manifest does not accept %q; use the matching push command", key)
		}
	}
	return parseManifest(raw, check)
}

// parseManifest parses a merchant config manifest. Bootstrap authority and
// catalog state are intentionally rejected by the strict YAML decoder.
func parseManifest[M any](raw []byte, check func(slug string, entry M) error) (*Manifest[M], error) {
	if err := RejectMisplacedMerchantConfigKeys(raw); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
	var manifest Manifest[M]
	if err := yaml.UnmarshalWithOptions(raw, &manifest, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("parse merchant config manifest: %w", err)
	}
	if len(manifest.Merchants) == 0 {
		return nil, fmt.Errorf("merchant config manifest must declare at least one merchant")
	}
	if manifest.Version != BootstrapManifestVersion {
		return nil, fmt.Errorf("merchant bootstrap: manifest version must be %d", BootstrapManifestVersion)
	}
	for _, rawSlug := range slices.Sorted(maps.Keys(manifest.Merchants)) {
		slug := strings.ToLower(strings.TrimSpace(rawSlug))
		if slug == "" {
			return nil, fmt.Errorf("merchant key is required")
		}
		if err := check(slug, manifest.Merchants[rawSlug]); err != nil {
			return nil, err
		}
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

// ProvisionMerchantParams provisions one declared merchant (#527). Standalone
// calls it with the control plane's merchant already registered; embedded
// calls it with Insert, registering an ownerless merchant row.
type ProvisionMerchantParams struct {
	// MerchantID is an already resolved, explicit host binding.
	MerchantID billing.MerchantID
	Config     *billing.Config
	Database   *db.DB
	// Merchants is the merchants service over the configuration cache the
	// declaration fills (a file) or seeds (Vault).
	Merchants *merchants.Service
	Slug      string
	Merchant  billing.MerchantDocument
	// Insert registers the merchant when it is missing.
	Insert bool
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
	case errors.Is(err, billing.ErrMerchantNotFound) && req.MerchantID.IsZero() && req.Insert:
		id, err := db.RegisterUnboundMerchant(ctx, req.Database.Qx(ctx), db.RegisterUnboundMerchantOptions{Slug: slug})
		if err != nil {
			return nil, nil, err
		}
		if tn, err = directory.Get(ctx, id); err != nil {
			return nil, nil, err
		}
	case errors.Is(err, billing.ErrMerchantNotFound) && req.MerchantID.IsZero():
		return nil, nil, fmt.Errorf("merchant bootstrap: merchant %q is missing; rerun with --insert to create it", slug)
	case err != nil:
		return nil, nil, fmt.Errorf("merchant bootstrap: lookup %q: %w", slug, err)
	}
	if req.Insert && req.MerchantID.IsZero() && tn.PermissionGroupID != "" {
		return nil, nil, fmt.Errorf("merchant bootstrap: %q is group-bound; an explicit verified binding is required", slug)
	}
	if refused, err = PutDeclaration(ctx, req, tn.ID, mt); err != nil {
		return nil, nil, fmt.Errorf("merchant bootstrap: configure %q: %w", slug, err)
	}
	return tn, refused, nil
}

// PutDeclaration puts a merchant's declared configuration in place when a
// file is the source and returns each document the merchant does not serve,
// with why: what its own checks refuse (ScopeDeclaration) and what the
// configuration's reconciliation rejects. One refused document never refuses the others. With Vault as the
// source there is nothing to put: a declaration naming configuration was
// refused before.
func PutDeclaration(ctx context.Context, req ProvisionMerchantParams, id billing.MerchantID, mt billing.MerchantDocument) (map[string]string, error) {
	cache := req.Merchants.Config()
	source, ok := cache.Source().(*merchantdocs.FileSource)
	if !ok {
		return nil, config.RefuseDeclarationBesideVault(req.Config, mt.Slug, mt)
	}
	mt, refused := ScopeDeclaration(req.Config, mt)
	set := merchantdocs.Declared(id, mt, time.Now().UTC())
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
// alone, and so does a messages document whose templates do not render. Settings that do not normalize, or a card custody the deployment
// cannot serve, refuse the merchant document and with it every PSP and
// custodian, which never serve without their merchant's settings.
func ScopeDeclaration(cfg *billing.Config, mt billing.MerchantDocument) (billing.MerchantDocument, map[string]string) {
	refused := map[string]string{}
	mt.PSPs, mt.Custodians = maps.Clone(mt.PSPs), maps.Clone(mt.Custodians)
	if mt.Messages != nil {
		if err := messages.Check(*mt.Messages); err != nil {
			refused[merchantdocs.MessagesDoc] = err.Error()
			mt.Messages = nil
		}
	}
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
		normalized, err := validatePSPDeclaration(key, account)
		if err != nil {
			refused[merchantdocs.PSPDoc(strings.ToLower(strings.TrimSpace(key)))] = err.Error()
			delete(mt.PSPs, key)
			continue
		}
		mt.PSPs[key] = normalized
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

func NormalizeManifestRail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateMerchantDeclaration refuses a declaration any of whose documents
// ScopeDeclaration would refuse: an operator command applies all of it or
// nothing.
func ValidateMerchantDeclaration(cfg *billing.Config, mt billing.MerchantDocument) error {
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
func validateMerchantDocument(cfg *billing.Config, mt billing.MerchantDocument) error {
	if _, err := merchantconfig.Normalize(mt.DisplayName, mt.MerchantSettings); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	declared := merchantdocs.Declared(billing.MerchantID{}, mt, time.Time{})
	var vault *billing.CardVaultConfig
	if cfg != nil && cfg.CardVault != (billing.CardVaultConfig{}) {
		vault = &cfg.CardVault
	}
	return merchants.CardVaultRefusal(vault, mt.MerchantSettings, declared.PSPs)
}

// validatePSPDeclaration checks one declared PSP and returns it in its
// declared environment: its rail, fields and environment, a CCBill account's
// FlexForm salt and a Stripe publishable key's environment.
func validatePSPDeclaration(key string, account billing.PSPConfig) (billing.PSPConfig, error) {
	account, err := config.NormalizePSPDeclaration(key, account)
	if err != nil {
		return account, err
	}
	rail := NormalizeManifestRail(string(account.Rail))
	// A CCBill account with inline credentials must sign its FlexForm links.
	if rail == string(models.RailCCBill) && len(account.Secrets) > 0 && strings.TrimSpace(account.Secrets["salt"]) == "" {
		return account, fmt.Errorf("psps.%s.secrets.salt is required", key)
	}
	if rail == string(models.RailStripe) {
		if raw, ok := account.Settings["publishable_key"]; ok {
			value, _ := raw.(string)
			if err := config.ValidateStripePublishableKeyEnvironment(string(account.Environment), value); err != nil {
				return account, fmt.Errorf("psps.%s.publishable_key: %w", key, err)
			}
		}
	}
	return account, nil
}
