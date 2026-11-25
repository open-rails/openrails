package merchantbootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/vaultconn"
)

// Merchants is a one-off process's merchant configuration: the merchants
// service over it, Vault Transit (the cranker's, when it signs there) and the
// cleanup.
type Merchants struct {
	Service       *merchants.Service
	SolanaTransit solana.TransitClient
	Close         func()
}

// OpenMerchants builds the merchant configuration a one-off process reads:
// Vault, over client, when a KV mount is named, else an empty file source the
// caller fills. The database handle reads settings through it.
func OpenMerchants(ctx context.Context, cfg *billing.Config, database *db.DB, client *vaultapi.Client) (*Merchants, error) {
	conn, err := vaultconn.Open(ctx, database.Log, cfg, client)
	if err != nil {
		return nil, err
	}
	if err := conn.State(); err != nil && conn.KV != nil {
		conn.Close()
		return nil, fmt.Errorf("vault holds merchant configuration: %w", err)
	}
	var source merchantdocs.Source = merchantdocs.NewFileSource()
	if conn.KV != nil {
		if source, err = merchantdocs.NewVaultSource(database.Log, conn.KV, cfg.Vault.ScopePrefix); err != nil {
			conn.Close()
			return nil, err
		}
	}
	cache := merchantdocs.NewCache(database.Log, source)
	closeAll := func() { cache.Close(); conn.Close() }
	svc, err := merchants.NewService(database.Log, database.DataPool(), cache, cfg)
	if err != nil {
		closeAll()
		return nil, err
	}
	svc.WithPlatformVaultStore(conn.PlatformKV)
	svc.StripeClients = stripeapi.NewFactory(nil, controls.View{DB: database})
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
	cache.SetSync(oneOffSync(svc))
	database.SetMerchantConfig(svc)
	return &Merchants{Service: svc, SolanaTransit: conn.SolanaTransit, Close: closeAll}, nil
}

// oneOffSync reconciles a loaded configuration and registers its Stripe PSPs'
// posture as a server does, so a one-off tool's write waits only for its PSP's
// first check.
func oneOffSync(svc *merchants.Service) merchantdocs.SyncFunc {
	return func(ctx context.Context, id billing.MerchantID, previous, loaded merchantdocs.Set) (merchantdocs.Set, error) {
		set, err := svc.SyncConfig(ctx, id, previous, loaded)
		if err == nil {
			merchants.RegisterStripePosture(svc.StripeClients, id, previous, set)
		}
		return set, err
	}
}

// OneOffMerchants builds the merchant configuration a one-off process reads PSPs
// through: Vault when a KV mount is named; else the merchant's entry in the
// manifest on disk fills a file source. No manifest and none at the
// conventional path leaves the merchant without configuration: nothing arms.
func OneOffMerchants(ctx context.Context, cfg *billing.Config, database *db.DB, client *vaultapi.Client, merchantID billing.MerchantID, manifest *BillingConfig, manifestPath string) (*Merchants, error) {
	opened, err := OpenMerchants(ctx, cfg, database, client)
	if err != nil {
		return nil, err
	}
	svc := opened.Service
	if _, ok := svc.Config().Source().(*merchantdocs.FileSource); ok {
		declaration, err := manifestEntry(ctx, database.Log, svc, merchantID, manifest, manifestPath)
		if err != nil {
			opened.Close()
			return nil, err
		}
		if declaration != nil {
			req := ProvisionMerchantParams{Config: cfg, Database: database, Merchants: svc}
			refused, err := PutDeclaration(ctx, req, merchantID, *declaration)
			if err == nil && len(refused) > 0 {
				err = rejected(refused)
			}
			if err != nil {
				opened.Close()
				return nil, fmt.Errorf("merchant %s configuration: %w", merchantID, err)
			}
		}
	}
	return opened, nil
}

// manifestEntry is the merchant's declaration in the manifest: the one given,
// else the file at manifestPath, else the conventional path when it exists.
func manifestEntry(ctx context.Context, logger *slog.Logger, directory *merchants.Service, merchantID billing.MerchantID, manifest *BillingConfig, manifestPath string) (*billing.MerchantDocument, error) {
	if manifest == nil {
		path := strings.TrimSpace(manifestPath)
		explicit := path != ""
		if !explicit {
			path = DefaultMerchantConfigManifestPath
		}
		raw, err := os.ReadFile(path) // #nosec G304 -- operator CLI flag or the fixed conventional path
		if errors.Is(err, os.ErrNotExist) && !explicit {
			logger.WarnContext(ctx, "no merchant manifest was supplied or found; the merchant has no configuration and no rail can be armed")
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read merchant manifest %s: %w", path, err)
		}
		if manifest, err = LoadManifest(raw, CheckManifestDeclaration); err != nil {
			return nil, fmt.Errorf("merchant manifest %s: %w", path, err)
		}
	}
	var selected *billing.MerchantDocument
	for name, mt := range manifest.Merchants {
		owner, err := directory.GetBySlug(ctx, name)
		if errors.Is(err, billing.ErrMerchantNotFound) {
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
