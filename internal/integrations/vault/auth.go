package vault

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/retry"
)

// Config configures the Vault client + auth for a managed deployment. It is kept
// in this package (not the global config) so self-hosted builds that never use
// Vault don't carry it; the composition root populates it from env/config.
type Config struct {
	// Address is the Vault server URL (VAULT_ADDR). Empty uses the api default.
	Address   string
	Namespace string
	// AuthMethod is "token", "approle", or "kubernetes". Empty defaults to
	// "token" when a Token is supplied, otherwise it is an error.
	AuthMethod string
	// Token is a pre-issued Vault token (VAULT_TOKEN). Used directly when
	// AuthMethod == "token" (dev / e2e). Renewed on the same watcher machinery
	// as approle/kubernetes when Vault reports it renewable (#751); otherwise
	// see Login's token-mode doc for the (no-recovery) failure posture.
	Token string
	// AppRole credentials (AuthMethod == "approle"). SecretID is re-resolved
	// fresh on every login/re-login attempt (#751 — see resolveApproleSecretID):
	// when it is delivered via the operator-mounted-secret-file convention
	// (config/secret_files.go — a file named VAULT_SECRET_ID), the CURRENT
	// file content is used every time, so a rotated secret_id is picked up
	// the moment re-auth needs it; otherwise this static value is used
	// unchanged.
	RoleID   string
	SecretID string
	// Kubernetes auth (AuthMethod == "kubernetes").
	K8sRole         string
	K8sJWTPath      string // defaults to the in-cluster service-account token path
	KubernetesMount string // defaults to "kubernetes"
	AppRoleMount    string // defaults to "approle"

}

const defaultK8sJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// reauthSecretIDEnvVar is the operator-mounted-secret-file name (filename =
// env-var name, config/secret_files.go's convention) resolveApproleSecretID
// re-reads on every login/re-login attempt when the approle secret_id is
// file-delivered.
const reauthSecretIDEnvVar = "VAULT_SECRET_ID"

// ErrNotAuthenticated means this process holds no working Vault token yet (or
// any longer). Operations refuse without a network call until the background
// login succeeds.
var ErrNotAuthenticated = fmt.Errorf("%w: not authenticated", ErrUnavailable)

// ErrSignerUnapproved means a Transit signer's key no longer matches the
// Solana identity stored for it (a rotated key, or the wrong Vault address,
// namespace or mount). The Solana rail refuses (503) until an operator
// approves the new identity.
var ErrSignerUnapproved = fmt.Errorf("%w: Solana signer identity changed and awaits operator approval", ErrUnavailable)

// Login builds the process Vault client (NOT per-merchant: merchant isolation
// is the (tenant, name) addressing) without touching the network. The returned
// Supervisor logs in, renews and re-authenticates in the background with
// capped full-jitter backoff until ctx ends; AuthState reports
// ErrNotAuthenticated until the first login succeeds. Only a configuration
// error (unknown method, token auth without a token) fails here.
func Login(ctx context.Context, cfg Config) (*vaultapi.Client, *Supervisor, error) {
	apiCfg := vaultapi.DefaultConfig()
	if strings.TrimSpace(cfg.Address) != "" {
		apiCfg.Address = cfg.Address
	}
	apiCfg.Timeout = requestTimeout
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: new client: %w", err)
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	method := strings.ToLower(strings.TrimSpace(cfg.AuthMethod))
	if method == "" && strings.TrimSpace(cfg.Token) != "" {
		method = "token"
	}
	switch method {
	case "token":
		// No ambient VAULT_TOKEN fallback (#712): env is read once at the binary
		// boundary (config vault.token maps from VAULT_TOKEN); absence fails here.
		token := strings.TrimSpace(cfg.Token)
		if token == "" {
			return nil, nil, fmt.Errorf("vault: token auth selected but no token (set vault.token; env VAULT_TOKEN feeds it via config.Load)")
		}
		client.SetToken(token)
	case "approle", "kubernetes":
		cfg.AuthMethod = method
	default:
		return nil, nil, fmt.Errorf("vault: unsupported auth method %q", cfg.AuthMethod)
	}
	sup := &Supervisor{client: client, cfg: cfg, reauthable: method != "token", kick: make(chan struct{}, 1), err: ErrNotAuthenticated}
	go sup.run(ctx)
	return client, sup, nil
}

const requestTimeout = 10 * time.Second

func login(ctx context.Context, client *vaultapi.Client, cfg Config) (*vaultapi.Secret, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.AuthMethod)) {
	case "approle":
		mount := firstNonEmpty(cfg.AppRoleMount, "approle")
		secretID, err := resolveApproleSecretID(cfg.SecretID)
		if err != nil {
			return nil, fmt.Errorf("vault: resolve approle secret_id: %w", err)
		}
		return client.Logical().WriteWithContext(ctx, "auth/"+mount+"/login", map[string]any{
			"role_id":   cfg.RoleID,
			"secret_id": secretID,
		})
	case "kubernetes":
		// Re-read from disk on EVERY call (including every re-auth attempt):
		// kubelet rotates this file in place, so re-reading is what lets
		// re-login (#751) pick up a rotated service-account token instead of
		// replaying whatever was on disk at process boot.
		jwtPath := firstNonEmpty(cfg.K8sJWTPath, defaultK8sJWTPath)
		jwt, err := os.ReadFile(jwtPath) // #nosec G304 -- jwtPath is operator config or the fixed k8s service-account token convention path
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

// resolveApproleSecretID resolves the CURRENT approle secret_id on every
// login/re-login attempt (#751 task 2), mirroring config.Load's own
// operator-mounted-secret-file convention (config/secret_files.go: a file
// named after the env var, typically rendered by a Vault Agent template or a
// CSI driver into /vault/secrets):
//
//   - a VAULT_SECRET_ID file under the mounted secrets dir: its CURRENT
//     content, re-read fresh every call via config.SecretFiles() (the SAME
//     accessor config.Load uses), so operator rotation is visible to the
//     very next re-auth attempt;
//   - otherwise: staticFallback, the value config.Load already resolved once
//     at boot (an env var or a literal config value — either way frozen for
//     the process lifetime, so there is nothing to re-read).
//
// This deliberately does NOT re-check the env var directly (#712: env is
// read exactly once, at the binary boundary, by the config package — no
// other package may call os.Getenv). Practically this never matters: the
// file and env conventions are alternative delivery mechanisms for the same
// slot, not meant to be set together. In the pathological case where an
// operator sets both, config.Load's boot-time precedence (env wins) governs
// the INITIAL login; a re-auth that finds the file still mounted will use
// its content rather than the frozen env value — a documented, accepted
// corner case, not a silent bug.
func resolveApproleSecretID(staticFallback string) (string, error) {
	files, err := config.SecretFiles()
	if err != nil {
		return "", fmt.Errorf("re-read mounted secret files: %w", err)
	}
	if v, ok := files[reauthSecretIDEnvVar]; ok {
		return v, nil
	}
	return staticFallback, nil
}

// Supervisor keeps one Login'd client authenticated for the life of the ctx
// Login was called with (#751): it logs in, renews the token/lease up to
// Vault's MAX TTL, and re-authenticates with the same method (re-reading
// credential material each attempt) when renewal ends or a permission-denied
// read proves the token dead. A bare static token has no login material, so
// it is only renewed while Vault allows. Every retry uses capped full-jitter
// backoff, forever. A fresh token is swapped onto the SAME *vaultapi.Client
// (SetToken is guarded by the client's own lock), so consumers never
// re-fetch it.
type Supervisor struct {
	client     *vaultapi.Client
	cfg        Config
	reauthable bool

	mu  sync.Mutex
	err error

	// kick ends the current watch early (NotifyPermissionDenied); buffered(1)
	// coalesces bursts.
	kick chan struct{}
}

// AuthState reports the Supervisor's current health: nil while Vault auth is
// believed good, the most recent re-auth/detection failure otherwise. Safe to
// call on a nil *Supervisor (reports healthy) so callers that discarded it
// (one-off test clients) don't need a nil check.
func (s *Supervisor) AuthState() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Supervisor) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// NotifyPermissionDenied lets a consumer (KVv2Adapter.WithReauthTrigger wires
// this onto the KV adapter) report a Vault permission-denied response it just
// observed, so a dead token is caught immediately instead of waiting out the
// current lease/renewal window (#751 task 5) — turning the old 15-MINUTE-LATE,
// cache-driven surprise into an immediate, correctly-attributed one.
//
// A 403 alone does not mean the TOKEN is dead: Vault returns the identical
// permission-denied response for a live, healthy token reading a path its
// policy simply doesn't grant. The distinguishing signal this error shape
// allows: re-run a self-lookup with the SAME token. If the self-lookup ALSO
// fails, the token itself is gone — trigger re-auth. If the self-lookup
// succeeds, the token is alive and the original 403 was a legitimate policy
// boundary on that one path; re-authenticating would reproduce the identical
// policy and accomplish nothing, so this is a deliberate no-op (never treat a
// scoped authorization denial as an auth-health problem).
func (s *Supervisor) NotifyPermissionDenied(err error) {
	if s == nil || s.client == nil || !IsPermissionDenied(err) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, lookupErr := s.client.Auth().Token().LookupSelfWithContext(ctx); lookupErr != nil {
		deadErr := fmt.Errorf("vault: auth token appears dead — a permission-denied read (%v) was followed by a failed self-lookup on the same token (%w)", err, lookupErr)
		log.WithError(deadErr).Error("vault: detected auth-token death from a runtime read; triggering immediate re-auth")
		s.setErr(deadErr)
		select {
		case s.kick <- struct{}{}:
		default: // a kick is already pending/being handled
		}
		return
	}
	log.WithError(err).Debug("vault: permission-denied read, but the token itself still checks out (self-lookup ok) — treating as an in-policy denial, not a re-auth trigger")
}

func (s *Supervisor) run(ctx context.Context) {
	failures := 0
	for ctx.Err() == nil {
		secret, err := s.authenticate(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setErr(fmt.Errorf("%w: %w", ErrNotAuthenticated, err))
			entry := log.WithError(err).WithField("attempt", failures+1)
			if failures == 0 {
				entry.Error("vault: authentication failed; Vault-backed signing and secrets are unavailable, retrying in the background")
			} else {
				entry.Debug("vault: authentication retry failed")
			}
			if !retry.Sleep(ctx, retry.Backoff(failures, time.Second, retry.Max)) {
				return
			}
			failures++
			continue
		}
		select {
		case <-s.kick:
		default:
		}
		s.setErr(nil)
		log.WithField("attempts", failures+1).Info("vault: authenticated")
		failures = 0
		s.hold(ctx, secret)
		if ctx.Err() != nil {
			return
		}
		if s.AuthState() == nil {
			s.setErr(fmt.Errorf("%w: token lease ended", ErrNotAuthenticated))
		}
		if !s.reauthable {
			log.Error("vault: static Vault token expired or was revoked; Vault access fails until the operator issues a fresh token (vault.token / VAULT_TOKEN)")
		}
	}
}

// authenticate obtains a working token: a fresh login for AppRole/Kubernetes,
// a self-lookup of the supplied token otherwise. A nil secret is a
// non-expiring token with nothing to renew.
func (s *Supervisor) authenticate(ctx context.Context) (*vaultapi.Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if s.reauthable {
		secret, err := login(ctx, s.client, s.cfg)
		if err != nil {
			return nil, err
		}
		if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
			return nil, fmt.Errorf("vault: login returned no token")
		}
		s.client.SetToken(secret.Auth.ClientToken)
		return secret, nil
	}
	lookup, err := s.client.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: token auth: look up token self: %w", err)
	}
	renewable, err := lookup.TokenIsRenewable()
	if err != nil {
		return nil, fmt.Errorf("vault: token auth: parse renewable: %w", err)
	}
	ttl, err := lookup.TokenTTL()
	if err != nil {
		return nil, fmt.Errorf("vault: token auth: parse ttl: %w", err)
	}
	token := s.client.Token()
	switch {
	case renewable:
		return &vaultapi.Secret{Auth: &vaultapi.SecretAuth{ClientToken: token, Renewable: true, LeaseDuration: int(ttl.Seconds())}}, nil
	case ttl > 0:
		log.Warnf("vault: token auth uses a NON-RENEWABLE token expiring at %s (in %s); rotate vault.token/VAULT_TOKEN before then", time.Now().Add(ttl).Format(time.RFC3339), ttl.Round(time.Second))
		return &vaultapi.Secret{Auth: &vaultapi.SecretAuth{ClientToken: token, LeaseDuration: int(ttl.Seconds())}}, nil
	default:
		return nil, nil
	}
}

// hold keeps secret alive until its lease ends, a kick proves it dead, or ctx
// ends. A nil secret never expires on its own.
func (s *Supervisor) hold(ctx context.Context, secret *vaultapi.Secret) {
	if secret == nil {
		select {
		case <-ctx.Done():
		case <-s.kick:
		}
		return
	}
	s.watchOnce(ctx, secret)
}

// Wait blocks until authenticated, up to timeout: for one-off tools that
// need Vault before doing anything, never for a serving process.
func (s *Supervisor) Wait(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		err := s.AuthState()
		if err == nil {
			return nil
		}
		if !retry.Sleep(ctx, 25*time.Millisecond) {
			return err
		}
	}
}

// Probe is the live check a host dependency supervisor runs: nil only while
// authenticated and Vault answers a token self-lookup.
func (s *Supervisor) Probe(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.AuthState(); err != nil {
		return err
	}
	if _, err := s.client.Auth().Token().LookupSelfWithContext(ctx); err != nil {
		if IsPermissionDenied(err) {
			s.NotifyPermissionDenied(err)
		}
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// watchOnce runs a LifetimeWatcher for secret to completion: naturally (the
// watcher's DoneCh, at Vault's MAX TTL or on a non-renewable lease), on ctx
// cancellation, or on an external kick (NotifyPermissionDenied) proving the
// token is already dead, in which case there is no point waiting out its
// nominal remaining lease.
func (s *Supervisor) watchOnce(ctx context.Context, secret *vaultapi.Secret) {
	watcher, err := s.client.NewLifetimeWatcher(&vaultapi.LifetimeWatcherInput{Secret: secret})
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
		case <-s.kick:
			return
		case err := <-watcher.DoneCh():
			if err != nil {
				log.WithError(err).Info("vault: token renewal ended; re-authenticating")
			}
			return
		case <-watcher.RenewCh():
			// renewed; still healthy.
		}
	}
}

// IsPermissionDenied reports whether err is a Vault permission-denied
// response (HTTP 403) — the shape NotifyPermissionDenied's distinguishing
// check is built on. Exported so other callers wrapping Vault operations can
// reuse the SAME classification instead of re-guessing at error text.
func IsPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	var rerr *vaultapi.ResponseError
	if errors.As(err, &rerr) {
		return rerr.StatusCode == http.StatusForbidden
	}
	return strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
