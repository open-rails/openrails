package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	vaultapi "github.com/hashicorp/vault/api"
	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

const (
	issuerRetryInterval        = 10 * time.Second
	tenantManifestAdvisoryLock = int64(734252042137424)
)

type TenantManifest struct {
	Version int              `yaml:"version"`
	Tenants []ManifestTenant `yaml:"tenants"`
}

type ManifestTenant struct {
	Slug          string                 `yaml:"slug"`
	Name          string                 `yaml:"name"`
	BillingTier   string                 `yaml:"billing_tier"`
	Region        string                 `yaml:"region"`
	WebhookHost   string                 `yaml:"webhook_host"`
	WebhookPath   string                 `yaml:"webhook_path"`
	Issuers       []ManifestIssuer       `yaml:"issuers"`
	ServiceTokens []ManifestServiceToken `yaml:"service_tokens"`
}

type ManifestIssuer struct {
	Issuer    string   `yaml:"issuer"`
	JWKSURI   string   `yaml:"jwks_uri"`
	Audiences []string `yaml:"audiences"`
	Enabled   *bool    `yaml:"enabled"`
}

type ManifestServiceToken struct {
	Name        string             `yaml:"name"`
	Permissions []string           `yaml:"permissions"`
	Resources   []ManifestResource `yaml:"resources"`
	Outputs     []ManifestOutput   `yaml:"outputs"`
}

type ManifestResource struct {
	Kind string `yaml:"kind"`
	ID   string `yaml:"id"`
}

type ManifestOutput struct {
	File  *ManifestFileOutput  `yaml:"file"`
	Vault *ManifestVaultOutput `yaml:"vault"`
}

type ManifestFileOutput struct {
	Path string `yaml:"path"`
}

type ManifestVaultOutput struct {
	Address string `yaml:"address"`
	Token   string `yaml:"token"`
	Mount   string `yaml:"mount"`
	Path    string `yaml:"path"`
	Field   string `yaml:"field"`
}

// ReconcileTenantManifest loads cfg.tenant_bootstrap.file, if configured, and
// applies the deployment-declared tenant state. Tenant rows and service-token
// outputs are reconciled synchronously. Issuer registration is retried in the background
// because registration validates JWKS reachability, and app pods may not be
// reachable yet during OpenRails startup.
func ReconcileTenantManifest(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane) error {
	path := tenantManifestPath(cfg)
	if path == "" {
		return nil
	}
	if cp == nil || cp.Core() == nil || cp.Pool() == nil {
		return fmt.Errorf("tenant bootstrap manifest configured but control plane is not enabled")
	}
	if err := lockTenantManifestBootstrap(ctx, cp); err != nil {
		return err
	}
	defer unlockTenantManifestBootstrap(context.Background(), cp)

	manifest, err := loadTenantManifest(path)
	if err != nil {
		return err
	}
	if len(manifest.Tenants) == 0 {
		log.WithField("file", path).Info("tenant bootstrap manifest has no tenants")
		return nil
	}

	svc, err := tenancy.NewService(cp.Pool(), cp, nil)
	if err != nil {
		return err
	}

	for _, mt := range manifest.Tenants {
		tn, err := svc.Provision(ctx, tenancy.ProvisionRequest{
			Slug:        mt.Slug,
			Name:        mt.Name,
			BillingTier: mt.BillingTier,
			Region:      mt.Region,
			WebhookHost: mt.WebhookHost,
			WebhookPath: mt.WebhookPath,
		})
		if err != nil {
			return fmt.Errorf("tenant bootstrap: provision %q: %w", mt.Slug, err)
		}
		log.WithFields(log.Fields{
			"tenant":    tn.Slug,
			"tenant_id": tn.ID.String(),
		}).Info("tenant bootstrap: tenant ensured")

		for _, token := range mt.ServiceTokens {
			if err := ensureManifestServiceToken(ctx, cfg, cp, tn.AuthKitTenantSlug, tn.ID, tn.Slug, token); err != nil {
				return fmt.Errorf("tenant bootstrap: service token %q for tenant %q: %w", token.Name, tn.Slug, err)
			}
		}
	}

	go reconcileManifestIssuersUntilReady(ctx, cp, manifest)
	return nil
}

func lockTenantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) error {
	_, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_lock($1)`, tenantManifestAdvisoryLock)
	if err != nil {
		return fmt.Errorf("tenant bootstrap: acquire advisory lock: %w", err)
	}
	return nil
}

func unlockTenantManifestBootstrap(ctx context.Context, cp *controlplane.ControlPlane) {
	if _, err := cp.Pool().Exec(ctx, `SELECT pg_advisory_unlock($1)`, tenantManifestAdvisoryLock); err != nil {
		log.WithError(err).Warn("tenant bootstrap: release advisory lock failed")
	}
}

func tenantManifestPath(cfg *config.Config) string {
	if cfg != nil && cfg.TenantBootstrap != nil {
		if p := strings.TrimSpace(cfg.TenantBootstrap.File); p != "" {
			return p
		}
	}
	return strings.TrimSpace(os.Getenv("OPENRAILS_TENANTS_FILE"))
}

func loadTenantManifest(path string) (*TenantManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tenant bootstrap: read %s: %w", path, err)
	}
	var manifest TenantManifest
	if err := yaml.UnmarshalWithOptions(raw, &manifest, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("tenant bootstrap: parse %s: %w", path, err)
	}
	if manifest.Version != 2 {
		return nil, fmt.Errorf("tenant bootstrap: manifest version must be 2")
	}
	return &manifest, nil
}

func ensureManifestServiceToken(ctx context.Context, cfg *config.Config, cp *controlplane.ControlPlane, ownerSlug string, tenantID tenant.ID, tenantSlug string, token ManifestServiceToken) error {
	name := strings.TrimSpace(token.Name)
	if name == "" {
		return errors.New("name is required")
	}
	ownerSlug = strings.TrimSpace(ownerSlug)
	if ownerSlug == "" {
		return errors.New("service token owner is required")
	}
	if len(token.Outputs) == 0 {
		return errors.New("at least one service token output is required")
	}

	if existing, err := readExistingServiceTokenOutput(ctx, cfg, token.Outputs); err != nil {
		return err
	} else if strings.TrimSpace(existing) != "" {
		if err := writeServiceTokenOutputs(ctx, cfg, token.Outputs, existing); err != nil {
			return err
		}
		log.WithFields(log.Fields{"tenant": tenantSlug, "service_token": name}).Info("tenant bootstrap: service token output already populated")
		return nil
	}

	permissions := cleanStrings(token.Permissions)
	if len(permissions) == 0 {
		return fmt.Errorf("permissions are required for service token %q", name)
	}
	resources, err := resolveManifestResources(token.Resources, tenantID, tenantSlug)
	if err != nil {
		return err
	}
	created, secret, err := cp.Core().MintServiceTokenWithOptions(ctx, ownerSlug, authcore.ServiceTokenMintOptions{
		Name:        name,
		Permissions: permissions,
		Resources:   resources,
	})
	if err != nil {
		return err
	}
	if err := writeServiceTokenOutputs(ctx, cfg, token.Outputs, secret); err != nil {
		return err
	}
	log.WithFields(log.Fields{"tenant": tenantSlug, "service_token": name, "token_key_id": created.KeyID}).Info("tenant bootstrap: service token minted and output written")
	return nil
}

func resolveManifestResources(in []ManifestResource, tenantID tenant.ID, tenantSlug string) ([]authcore.ServiceTokenResource, error) {
	if len(in) == 0 {
		return nil, errors.New("resources are required for service token")
	}
	out := make([]authcore.ServiceTokenResource, 0, len(in))
	for _, r := range in {
		kind := strings.TrimSpace(r.Kind)
		id := strings.TrimSpace(r.ID)
		if kind == "" || id == "" {
			return nil, errors.New("resource kind and id are required")
		}
		if kind == controlplane.ResourceKindTenant {
			switch strings.ToLower(id) {
			case "$tenant", "self", tenantSlug:
				id = tenantID.String()
			}
		}
		out = append(out, authcore.ServiceTokenResource{Kind: kind, ID: id})
	}
	return out, nil
}

func readExistingServiceTokenOutput(ctx context.Context, cfg *config.Config, outputs []ManifestOutput) (string, error) {
	for _, out := range outputs {
		token, err := readOneServiceTokenOutput(ctx, cfg, out)
		if err != nil || strings.TrimSpace(token) != "" {
			return token, err
		}
	}
	return "", nil
}

func readOneServiceTokenOutput(ctx context.Context, cfg *config.Config, out ManifestOutput) (string, error) {
	if out.File != nil && strings.TrimSpace(out.File.Path) != "" {
		raw, err := os.ReadFile(out.File.Path)
		if err == nil {
			return strings.TrimSpace(string(raw)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if out.Vault != nil && strings.TrimSpace(out.Vault.Path) != "" && strings.TrimSpace(out.Vault.Field) != "" {
		data, err := readVaultData(ctx, cfg, *out.Vault)
		if err != nil {
			return "", err
		}
		if v, ok := data[strings.TrimSpace(out.Vault.Field)].(string); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", nil
}

func writeServiceTokenOutputs(ctx context.Context, cfg *config.Config, outputs []ManifestOutput, token string) error {
	for _, out := range outputs {
		if err := writeOneServiceTokenOutput(ctx, cfg, out, token); err != nil {
			return err
		}
	}
	return nil
}

func writeOneServiceTokenOutput(ctx context.Context, cfg *config.Config, out ManifestOutput, token string) error {
	wrote := false
	if out.File != nil && strings.TrimSpace(out.File.Path) != "" {
		path := strings.TrimSpace(out.File.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			return err
		}
		wrote = true
	}
	if out.Vault != nil && strings.TrimSpace(out.Vault.Path) != "" && strings.TrimSpace(out.Vault.Field) != "" {
		data, err := readVaultData(ctx, cfg, *out.Vault)
		if err != nil {
			return err
		}
		data[strings.TrimSpace(out.Vault.Field)] = token
		if err := writeVaultData(ctx, cfg, *out.Vault, data); err != nil {
			return err
		}
		wrote = true
	}
	if !wrote {
		return errors.New("service token output requires file.path or vault.path/vault.field")
	}
	return nil
}

func reconcileManifestIssuersUntilReady(ctx context.Context, cp *controlplane.ControlPlane, manifest *TenantManifest) {
	ticker := time.NewTicker(issuerRetryInterval)
	defer ticker.Stop()

	for {
		done := reconcileManifestIssuersOnce(ctx, cp, manifest)
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reconcileManifestIssuersOnce(ctx context.Context, cp *controlplane.ControlPlane, manifest *TenantManifest) bool {
	allDone := true
	for _, mt := range manifest.Tenants {
		if len(mt.Issuers) == 0 {
			continue
		}
		tid, err := tenantIDForSlug(ctx, cp, mt.Slug)
		if err != nil {
			log.WithError(err).WithField("tenant", mt.Slug).Warn("tenant bootstrap: issuer reconciliation waiting for tenant")
			allDone = false
			continue
		}
		for _, issuer := range mt.Issuers {
			if issuer.Enabled != nil && !*issuer.Enabled {
				if err := cp.DisableDelegatedIssuer(ctx, tid, issuer.Issuer); err != nil {
					log.WithError(err).WithField("issuer", issuer.Issuer).Warn("tenant bootstrap: disable issuer failed")
					allDone = false
				}
				continue
			}
			err := cp.RegisterDelegatedIssuer(ctx, controlplane.RegisterDelegatedIssuerParams{
				TenantID:  tid,
				Issuer:    issuer.Issuer,
				JWKSURI:   issuer.JWKSURI,
				Audiences: issuer.Audiences,
			})
			if err != nil {
				log.WithError(err).WithFields(log.Fields{"tenant": mt.Slug, "issuer": issuer.Issuer, "jwks_uri": issuer.JWKSURI}).
					Warn("tenant bootstrap: issuer reconciliation will retry")
				allDone = false
				continue
			}
			log.WithFields(log.Fields{"tenant": mt.Slug, "issuer": issuer.Issuer}).Info("tenant bootstrap: issuer registered")
		}
	}
	return allDone
}

func tenantIDForSlug(ctx context.Context, cp *controlplane.ControlPlane, slug string) (tenant.ID, error) {
	var id string
	err := cp.Pool().QueryRow(ctx, `
		SELECT id::text FROM billing.tenants
		 WHERE slug = $1 AND status = 'active' AND deleted_at IS NULL
		 LIMIT 1
	`, strings.ToLower(strings.TrimSpace(slug))).Scan(&id)
	if err != nil {
		return tenant.ID{}, err
	}
	return tenant.ParseID(id)
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func vaultClient(cfg *config.Config, out ManifestVaultOutput) (*vaultapi.Client, string, error) {
	address := strings.TrimSpace(out.Address)
	token := strings.TrimSpace(out.Token)
	mount := strings.Trim(strings.TrimSpace(out.Mount), "/")
	if cfg != nil && cfg.Vault != nil {
		if address == "" {
			address = strings.TrimSpace(cfg.Vault.Address)
		}
		if token == "" {
			token = strings.TrimSpace(cfg.Vault.Token)
		}
		if mount == "" {
			mount = strings.Trim(strings.TrimSpace(cfg.Vault.KVMount), "/")
		}
	}
	if address == "" {
		address = strings.TrimSpace(os.Getenv("VAULT_ADDR"))
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv("VAULT_TOKEN"))
	}
	if mount == "" {
		mount = strings.Trim(strings.TrimSpace(os.Getenv("VAULT_KV_MOUNT")), "/")
	}
	if mount == "" {
		mount = "secret"
	}
	if address == "" || token == "" {
		return nil, "", errors.New("vault service token output requires VAULT_ADDR and VAULT_TOKEN")
	}
	vcfg := vaultapi.DefaultConfig()
	vcfg.Address = address
	client, err := vaultapi.NewClient(vcfg)
	if err != nil {
		return nil, "", err
	}
	client.SetToken(token)
	return client, mount, nil
}

func readVaultData(ctx context.Context, cfg *config.Config, out ManifestVaultOutput) (map[string]any, error) {
	client, mount, err := vaultClient(cfg, out)
	if err != nil {
		return nil, err
	}
	secret, err := client.Logical().ReadWithContext(ctx, mount+"/data/"+strings.Trim(strings.TrimSpace(out.Path), "/"))
	if err != nil {
		return nil, err
	}
	if secret == nil || secret.Data == nil {
		return map[string]any{}, nil
	}
	data, ok := secret.Data["data"].(map[string]any)
	if !ok || data == nil {
		return map[string]any{}, nil
	}
	return data, nil
}

func writeVaultData(ctx context.Context, cfg *config.Config, out ManifestVaultOutput, data map[string]any) error {
	client, mount, err := vaultClient(cfg, out)
	if err != nil {
		return err
	}
	_, err = client.Logical().WriteWithContext(ctx, mount+"/data/"+strings.Trim(strings.TrimSpace(out.Path), "/"), map[string]any{"data": data})
	return err
}
