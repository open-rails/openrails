package vault

import (
	"context"
	"fmt"
	"os"
	"strings"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"
)

// Config configures the Vault client + auth for a managed deployment. It is kept
// in this package (not the global config) so self-hosted builds that never use
// Vault don't carry it; the composition root populates it from env/config.
type Config struct {
	// Address is the Vault server URL (VAULT_ADDR). Empty uses the api default.
	Address string
	// AuthMethod is "token", "approle", or "kubernetes". Empty defaults to
	// "token" when a Token is supplied, otherwise it is an error.
	AuthMethod string
	// Token is a pre-issued Vault token (VAULT_TOKEN). Used directly when
	// AuthMethod == "token" (dev / e2e). Not renewed by us — the operator owns it.
	Token string
	// AppRole credentials (AuthMethod == "approle").
	RoleID   string
	SecretID string
	// Kubernetes auth (AuthMethod == "kubernetes").
	K8sRole         string
	K8sJWTPath      string // defaults to the in-cluster service-account token path
	KubernetesMount string // defaults to "kubernetes"
	AppRoleMount    string // defaults to "approle"

}

const defaultK8sJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Login authenticates the OpenRails process to Vault (NOT per-merchant — merchant
// isolation is enforced by the (tenant, name) addressing) and returns a client
// whose token is kept fresh by a background renewer until ctx is cancelled.
func Login(ctx context.Context, cfg Config) (*vaultapi.Client, error) {
	apiCfg := vaultapi.DefaultConfig()
	if strings.TrimSpace(cfg.Address) != "" {
		apiCfg.Address = cfg.Address
	}
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault: new client: %w", err)
	}

	// Token auth short-circuits the login flow: the operator supplied a token
	// directly (dev server, e2e, or a sidecar-managed token). We set it and do
	// not run a renewer — its lifecycle is owned outside the process.
	method := strings.ToLower(strings.TrimSpace(cfg.AuthMethod))
	if method == "" && strings.TrimSpace(cfg.Token) != "" {
		method = "token"
	}
	if method == "token" {
		// No ambient VAULT_TOKEN fallback (#712): env is read once at the binary
		// boundary (config vault.token maps from VAULT_TOKEN); absence fails here.
		token := strings.TrimSpace(cfg.Token)
		if token == "" {
			return nil, fmt.Errorf("vault: token auth selected but no token (set vault.token; env VAULT_TOKEN feeds it via config.Load)")
		}
		client.SetToken(token)
		return client, nil
	}

	secret, err := login(ctx, client, cfg)
	if err != nil {
		return nil, err
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, fmt.Errorf("vault: login returned no token")
	}
	client.SetToken(secret.Auth.ClientToken)

	if secret.Auth.Renewable {
		go renew(ctx, client, secret)
	}
	return client, nil
}

func login(ctx context.Context, client *vaultapi.Client, cfg Config) (*vaultapi.Secret, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.AuthMethod)) {
	case "approle":
		mount := firstNonEmpty(cfg.AppRoleMount, "approle")
		return client.Logical().WriteWithContext(ctx, "auth/"+mount+"/login", map[string]any{
			"role_id":   cfg.RoleID,
			"secret_id": cfg.SecretID,
		})
	case "kubernetes":
		jwtPath := firstNonEmpty(cfg.K8sJWTPath, defaultK8sJWTPath)
		jwt, err := os.ReadFile(jwtPath)
		if err != nil {
			return nil, fmt.Errorf("vault: read k8s service-account token: %w", err)
		}
		mount := firstNonEmpty(cfg.KubernetesMount, "kubernetes")
		return client.Logical().WriteWithContext(ctx, "auth/"+mount+"/login", map[string]any{
			"role": cfg.K8sRole,
			"jwt":  strings.TrimSpace(string(jwt)),
		})
	default:
		return nil, fmt.Errorf("vault: unsupported auth method %q", cfg.AuthMethod)
	}
}

// renew keeps the auth token alive; on terminal failure it logs so the operator
// (or a supervisor calling Login again) can react. It never panics.
func renew(ctx context.Context, client *vaultapi.Client, secret *vaultapi.Secret) {
	watcher, err := client.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{Secret: secret})
	if err != nil {
		log.WithError(err).Warn("vault: could not start token renewer")
		return
	}
	go watcher.Start()
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-watcher.DoneCh():
			if err != nil {
				log.WithError(err).Warn("vault: token renewal stopped; re-auth required")
			}
			return
		case <-watcher.RenewCh():
			// renewed; continue
		}
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
