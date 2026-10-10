package merchantbootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/vaultconn"
)

// Merchants is a one-off process's merchant configuration: the merchants
// service over it, the Transit signer and the cleanup.
type Merchants struct {
	Service       *merchants.Service
	SolanaTransit solana.TransitClient
	Close         func()
}

// OpenMerchants builds the merchant configuration a one-off process reads:
// Vault when a KV mount is named (awaiting its login), else an empty file
// source the caller fills. The database handle reads settings through it.
func OpenMerchants(ctx context.Context, cfg *config.Config, database *db.DB) (*Merchants, error) {
	conn, err := vaultconn.Open(ctx, cfg, nil)
	if err != nil {
		return nil, err
	}
	if cfg.Vault != nil {
		if err := conn.Await(ctx, vaultconn.AwaitTimeout); err != nil {
			conn.Close()
			return nil, err
		}
	}
	var source merchantdocs.Source = merchantdocs.NewFileSource()
	if conn.KV != nil {
		if source, err = merchantdocs.NewVaultSource(conn.KV, cfg.Vault.ScopePrefix); err != nil {
			conn.Close()
			return nil, err
		}
	}
	cache := merchantdocs.NewCache(source)
	closeAll := func() { cache.Close(); conn.Close() }
	svc, err := merchants.NewService(database.DataPool(), cache, config.ExpectedProviderEnvironment(config.IsTestMode(cfg)))
	if err != nil {
		closeAll()
		return nil, err
	}
	if vault, ok := source.(*merchantdocs.VaultSource); ok {
		key, err := vault.FingerprintKey(ctx)
		if err == nil {
			var fingerprints *merchants.CredentialFingerprinter
			if fingerprints, err = merchants.NewCredentialFingerprinter(key); err == nil {
				svc.WithCredentialFingerprinter(fingerprints)
			}
		}
		if err != nil {
			closeAll()
			return nil, err
		}
	}
	cache.SetSync(svc.SyncConfig)
	database.SetMerchantConfig(svc)
	return &Merchants{Service: svc, SolanaTransit: conn.SolanaTransit, Close: closeAll}, nil
}

// OneOffMerchants builds the merchants service a one-off process reads PSPs
// through: Vault when a KV mount is named; else the merchant's entry in the
// manifest on disk fills a file source. No manifest and none at the
// conventional path leaves the merchant without configuration: nothing arms.
func OneOffMerchants(ctx context.Context, cfg *config.Config, database *db.DB, merchantID billing.MerchantID, manifest *BillingConfig, manifestPath string, overlays []string) (*merchants.Service, func(), error) {
	opened, err := OpenMerchants(ctx, cfg, database)
	if err != nil {
		return nil, nil, err
	}
	svc := opened.Service
	if _, ok := svc.Config().Source().(*merchantdocs.FileSource); ok {
		declaration, err := manifestEntry(ctx, svc, merchantID, manifest, manifestPath, overlays)
		if err != nil {
			opened.Close()
			return nil, nil, err
		}
		if declaration != nil {
			req := ProvisionMerchantParams{Config: cfg, Database: database, Merchants: svc, SolanaTransit: opened.SolanaTransit}
			if err := PutDeclaration(ctx, req, merchantID, *declaration); err != nil {
				opened.Close()
				return nil, nil, fmt.Errorf("merchant %s configuration: %w", merchantID, err)
			}
		}
	}
	return svc, opened.Close, nil
}

// manifestEntry is the merchant's declaration in the manifest: the one given,
// else the file at manifestPath, else the conventional path when it exists.
func manifestEntry(ctx context.Context, directory *merchants.Service, merchantID billing.MerchantID, manifest *BillingConfig, manifestPath string, overlayPaths []string) (*config.MerchantDeclaration, error) {
	if manifest == nil {
		path := strings.TrimSpace(manifestPath)
		explicit := path != ""
		if !explicit {
			path = DefaultMerchantConfigManifestPath
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- operator CLI flag or the fixed conventional path
		if errors.Is(err, os.ErrNotExist) && !explicit {
			log.Warn("no merchant manifest was supplied or found; the merchant has no configuration and no rail can be armed")
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read merchant manifest %s: %w", path, err)
		}
		overlays, err := ReadMerchantManifestOverlays(overlayPaths)
		if err != nil {
			return nil, err
		}
		if manifest, err = LoadMerchantConfigManifestWithOverlays(raw, overlays...); err != nil {
			return nil, fmt.Errorf("merchant manifest %s: %w", path, err)
		}
	}
	var selected *config.MerchantDeclaration
	for name, mt := range manifest.Merchants {
		owner, err := directory.GetBySlug(ctx, name)
		if errors.Is(err, merchants.ErrMerchantNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve manifest merchant %q: %w", name, err)
		}
		if owner.ID != merchantID {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("multiple manifest names resolve to merchant %s", merchantID)
		}
		mt.Slug = name
		selected = &mt
	}
	if selected == nil {
		return nil, fmt.Errorf("the merchant manifest has no entry for merchant %s", merchantID)
	}
	return selected, nil
}
