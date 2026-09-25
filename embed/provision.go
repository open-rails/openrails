package embed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/goccy/go-yaml"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/vault"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
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
type PSPSignerConfig = boot.PSPSignerConfig

// upsertMerchantConfig reconciles the constructor's merchant declaration before
// HTTP routes or workers can observe a partially configured runtime. With
// tolerateVault, a Solana vault_transit signer whose Vault is unavailable
// reuses the identity stored for it, or (none stored yet) leaves only that PSP
// unprovisioned; the returned flag says Vault must still confirm it.
func upsertMerchantConfig(ctx context.Context, a *app.App, slug string, m MerchantConfig, tolerateVault bool) (merchant.ID, bool, error) {
	if a == nil || a.Runtime == nil || a.Runtime.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: app database not initialized")
	}
	conf := a.Config
	if conf == nil || conf.DB == nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: config/db is required")
	}
	database := a.Runtime.DB

	directory := a.Runtime.Merchants
	if directory == nil {
		var err error
		directory, err = merchants.NewDirectoryService(database.DataPool())
		if err != nil {
			return merchant.ID{}, false, err
		}
	}
	bound := a.Runtime.ConfiguredMerchant()
	if !bound.IsZero() {
		selected, err := directory.GetBySlug(ctx, merchant.NormalizeSlug(slug))
		if err != nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: engine is already bound to merchant %s; cannot resolve supplied name %q: %w", bound, slug, err)
		}
		if selected.ID != bound {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: one embedded engine serves one merchant; refusing second merchant %q (%s), bound to %s", slug, selected.ID, bound)
		}
		slug = selected.Slug
	}

	// Run it through the same billing-only merchant-provisioning boundary embed.New
	// and the bootstrap CLI use, with ControlPlane nil. Provider-account reconcile
	// needs a merchant secret store (built over the engine's own pool).
	req := boot.ProvisionMerchantRequest{
		Config:     conf,
		Database:   database,
		MerchantID: bound,
		Directory:  directory,
		Slug:       slug,
		Merchant:   m,
		Options:    boot.MerchantManifestReconcileOptions{Insert: true, StripeClients: a.Runtime.StripeClients},
	}
	var fallback *signerTransit
	switch {
	case conf.SecretStoreBackend() == config.SecretBackendSnapshot:
		// Load the host-owned snapshot into this process. Metadata initialization
		// is create-only; authorized edits and archived accounts survive restarts.
		if a.Runtime == nil || a.Runtime.ManifestSecrets == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: snapshot credentials require the runtime snapshot plane")
		}
		req.SecretStore = a.Runtime.ManifestSecrets.Seeder()
		backend := a.Runtime.MerchantSecretBackend
		if backend == nil {
			return merchant.ID{}, false, fmt.Errorf("openrails embed: credential backend must be initialized before provisioning")
		}
		req.SolanaTransit = backend.SolanaTransit
		if backend.SolanaTransit != nil {
			fallback = &signerTransit{TransitClient: backend.SolanaTransit, db: database, directory: directory, slug: slug,
				environment: config.ExpectedProviderEnvironment(conf.IsTestMode()), tolerate: tolerateVault, onChange: a.Runtime.ReportSignerKeyChange}
			req.SolanaTransit = fallback
			req.Options.DeferPSP = func(_ string, err error) bool {
				return errors.Is(err, vault.ErrSignerUnapproved) || (tolerateVault && errors.Is(err, vault.ErrUnavailable))
			}
		}
	case len(m.PSPs) > 0 || len(m.Custodians) > 0:
		return merchant.ID{}, false, fmt.Errorf("openrails embed: provider credential declarations require a host snapshot; use the Client payment-provider publication operation for managed credentials")
	}

	tn, err := boot.ProvisionMerchant(ctx, req)
	if err != nil {
		return merchant.ID{}, false, fmt.Errorf("openrails embed: upsert merchant config: %w", err)
	}
	if a.Runtime.ConfiguredMerchant().IsZero() {
		a.Runtime.SetConfiguredMerchant(tn.ID)
	}
	return tn.ID, fallback != nil && fallback.unavailable.Load(), nil
}

// signerTransit checks every Transit public-key read against the Solana
// identities stored for that key. A different key fails closed: the change is
// recorded on the stored PSP rows, the Solana rail refuses until an operator
// approves it (ApproveSolanaSigner), and the new identity is not provisioned.
// With tolerate, a read Vault cannot serve right now is answered from the
// stored identity.
type signerTransit struct {
	solanaint.TransitClient
	db          *db.DB
	directory   *merchants.Service
	slug        string
	environment string
	tolerate    bool
	onChange    func(error)
	unavailable atomic.Bool
}

func (t *signerTransit) PublicKey(ctx context.Context, key string) ([]byte, error) {
	pub, err := t.TransitClient.PublicKey(ctx, key)
	if err != nil && (!t.tolerate || !errors.Is(err, vault.ErrUnavailable)) {
		return nil, err
	}
	mid, rows := storedSigners(ctx, t.db, t.directory, t.slug, t.environment, key)
	if err != nil {
		t.unavailable.Store(true)
		if len(rows) == 0 {
			return nil, err
		}
		log.WithField("key", key).Warn("openrails embed: Vault unavailable; Solana signer uses its stored identity until Vault confirms it")
		return rows[len(rows)-1].identity.Bytes(), nil
	}
	if len(pub) != 32 || len(rows) == 0 {
		return pub, nil
	}
	current := solanago.PublicKeyFromBytes(pub)
	for _, r := range rows {
		if r.identity.Equals(current) {
			return pub, nil
		}
	}
	previous := rows[len(rows)-1].identity
	log.WithFields(log.Fields{"merchant_id": mid.String(), "key": key, "stored_public_key": previous.String(), "transit_public_key": current.String()}).
		Error("openrails embed: Vault Transit key changed; the Solana rail is refused until an operator approves the new identity")
	if err := markSignerChange(ctx, t.db, mid, rows, current.String()); err != nil {
		return nil, fmt.Errorf("record solana signer change: %w", err)
	}
	if t.onChange != nil {
		t.onChange(fmt.Errorf("solana signer %q changed from %s to %s; awaiting approval", key, previous, current))
	}
	return nil, fmt.Errorf("solana signer %q: %w", key, vault.ErrSignerUnapproved)
}

type storedSigner struct {
	row      gen.OpenrailsPsp
	identity solanago.PublicKey
}

// storedSigners returns the merchant and its active Solana PSPs signed by the
// Transit key, oldest first.
func storedSigners(ctx context.Context, database *db.DB, directory *merchants.Service, slug, environment, key string) (merchant.ID, []storedSigner) {
	m, err := directory.GetBySlug(ctx, merchant.NormalizeSlug(slug))
	if err != nil {
		return merchant.ID{}, nil
	}
	rail := "solana"
	var out []storedSigner
	_ = database.RunInMerchantScope(ctx, m.ID, "stored solana signer", func(ctx context.Context) error {
		rows, err := database.Gen(ctx).ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: m.ID.UUID(), Rail: &rail})
		if err != nil {
			return err
		}
		for _, row := range rows {
			var evidence struct {
				Signer struct{ Mode, Key string } `json:"signer"`
			}
			if row.Archived || row.Environment != environment || json.Unmarshal(row.Evidence, &evidence) != nil {
				continue
			}
			if evidence.Signer.Mode != "vault_transit" || evidence.Signer.Key != key {
				continue
			}
			if pub, err := solanago.PublicKeyFromBase58(row.AccountID); err == nil {
				out = append(out, storedSigner{row: row, identity: pub})
			}
		}
		return nil
	})
	return m.ID, out
}

// markSignerChange records the unapproved public key on the stored rows (or
// clears it with ""), optionally archiving them.
func markSignerChange(ctx context.Context, database *db.DB, mid merchant.ID, rows []storedSigner, publicKey string, archive ...bool) error {
	return database.RunInMerchantScope(ctx, mid, "solana signer change", func(ctx context.Context) error {
		for _, r := range rows {
			var evidence map[string]any
			if err := json.Unmarshal(r.row.Evidence, &evidence); err != nil || evidence == nil {
				evidence = map[string]any{}
			}
			if publicKey == "" {
				delete(evidence, "signer_change")
			} else {
				evidence["signer_change"] = map[string]any{"public_key": publicKey, "detected_at": time.Now().UTC().Format(time.RFC3339)}
			}
			raw, err := json.Marshal(evidence)
			if err != nil {
				return err
			}
			archived := r.row.Archived || (len(archive) > 0 && archive[0])
			environment := r.row.Environment
			if _, err := database.Gen(ctx).UpsertPSP(ctx, gen.UpsertPSPParams{
				ID: r.row.ID, MerchantID: r.row.MerchantID, Rail: r.row.Rail, Environment: &environment, AccountID: r.row.AccountID,
				Key: r.row.Key, Archived: &archived, Evidence: raw, LastVerifiedAt: r.row.LastVerifiedAt, CustodianID: r.row.CustodianID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// approveSolanaSigner accepts the identity Vault now reports for key: the
// stored identities awaiting it are drained (archived) and the declaration is
// re-applied, provisioning the approved one. It refuses when nothing is
// pending for exactly the key Vault reports.
func approveSolanaSigner(ctx context.Context, a *app.App, declaration *MerchantDeclaration, mid merchant.ID, key string) error {
	if declaration == nil {
		return fmt.Errorf("openrails embed: no merchant declaration to approve a signer for")
	}
	backend := a.Runtime.MerchantSecretBackend
	if backend == nil || backend.SolanaTransit == nil {
		return fmt.Errorf("openrails embed: no Vault Transit signer is configured")
	}
	pub, err := backend.SolanaTransit.PublicKey(ctx, key)
	if err != nil {
		return fmt.Errorf("read the Transit key: %w", err)
	}
	approved := solanago.PublicKeyFromBytes(pub).String()
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return err
	}
	m, err := directory.Get(ctx, mid)
	if err != nil {
		return err
	}
	environment := config.ExpectedProviderEnvironment(a.Config.IsTestMode())
	_, rows := storedSigners(ctx, a.Runtime.DB, directory, m.Slug, environment, key)
	var pending []storedSigner
	for _, r := range rows {
		if merchants.SignerChange(r.row.Evidence) == approved {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return fmt.Errorf("openrails embed: no signer change awaiting approval for key %q reporting %s", key, approved)
	}
	if err := markSignerChange(ctx, a.Runtime.DB, mid, pending, "", true); err != nil {
		return err
	}
	if _, _, err := upsertMerchantConfig(ctx, a, declaration.Slug, declaration.Config, false); err != nil {
		return err
	}
	a.Runtime.ReportSignerKeyChange(nil)
	log.WithFields(log.Fields{"merchant_id": mid.String(), "key": key, "public_key": approved}).Warn("openrails embed: operator approved a new Solana signer identity")
	return nil
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

// LoadMerchantConfigManifestWithOverlays merges the manifest with the host's
// structured YAML secret overlays (later wins) and validates the result. The
// host's config tree is the only source; the engine reads no env or files.
func LoadMerchantConfigManifestWithOverlays(raw []byte, overlays ...[]byte) (*BillingConfig, error) {
	return boot.LoadMerchantConfigManifestWithOverlays(raw, overlays...)
}
