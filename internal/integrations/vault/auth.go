package vault

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
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

// Login authenticates the OpenRails process to Vault (NOT per-merchant — merchant
// isolation is enforced by the (tenant, name) addressing) and returns a client
// whose token is kept fresh by a background Supervisor until ctx is cancelled.
//
// The returned Supervisor is the #751 "supervisor calling Login again" a stale
// comment on the old renew() promised but never built: it renews the current
// token/lease up to Vault's MAX TTL exactly as before, and now ALSO
// re-authenticates automatically when that is no longer possible (see
// Supervisor's doc). Callers that only need the client (tests standing up a
// one-off root/dedicated-container connection, for instance) may discard it;
// production callers should keep it to feed a readiness probe (AuthState) and
// to wire KVv2Adapter.WithReauthTrigger for immediate 403-triggered re-auth.
func Login(ctx context.Context, cfg Config) (*vaultapi.Client, *Supervisor, error) {
	apiCfg := vaultapi.DefaultConfig()
	if strings.TrimSpace(cfg.Address) != "" {
		apiCfg.Address = cfg.Address
	}
	client, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("vault: new client: %w", err)
	}

	// Token auth short-circuits the credential-login flow: the operator
	// supplied a token directly (dev server, e2e, or a sidecar-managed
	// token). There is no login material to redo if it ever dies, so its
	// Supervisor renews-if-renewable and otherwise only detects+alarms (#751
	// task 3) — see startStaticTokenSupervisor.
	method := strings.ToLower(strings.TrimSpace(cfg.AuthMethod))
	if method == "" && strings.TrimSpace(cfg.Token) != "" {
		method = "token"
	}
	if method == "token" {
		// No ambient VAULT_TOKEN fallback (#712): env is read once at the binary
		// boundary (config vault.token maps from VAULT_TOKEN); absence fails here.
		token := strings.TrimSpace(cfg.Token)
		if token == "" {
			return nil, nil, fmt.Errorf("vault: token auth selected but no token (set vault.token; env VAULT_TOKEN feeds it via config.Load)")
		}
		client.SetToken(token)
		sup, err := startStaticTokenSupervisor(ctx, client, token)
		if err != nil {
			return nil, nil, err
		}
		return client, sup, nil
	}

	secret, err := login(ctx, client, cfg)
	if err != nil {
		return nil, nil, err
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, nil, fmt.Errorf("vault: login returned no token")
	}
	client.SetToken(secret.Auth.ClientToken)

	sup := &Supervisor{client: client, cfg: cfg, reauthable: true, kick: make(chan struct{}, 1)}
	// Supervise unconditionally, even when the initial secret is (unusually)
	// non-renewable: the watcher still waits out its lease and signals done
	// at expiry, which is exactly the re-auth trigger (#751) — previously a
	// non-renewable initial secret meant NO watcher at all, and the token was
	// never refreshed by any means.
	go sup.superviseLoop(ctx, secret)
	return client, sup, nil
}

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

// Supervisor is the "supervisor calling Login again" a stale comment on the
// old renew() promised but never built (#751). It owns keeping ONE Login'd
// Vault client authenticated for the life of the ctx Login was called with:
//
//   - it runs Vault's own LifetimeWatcher to renew the current token/lease up
//     to its Vault-enforced MAX TTL — unchanged from before this issue;
//   - when the watcher ends (MAX TTL reached, the secret was never
//     renewable, or NotifyPermissionDenied confirms the token already died),
//     it re-authenticates with the SAME credential method, re-resolving
//     credential material fresh on every attempt (see resolveApproleSecretID
//     and the kubernetes JWT re-read in login()), backing off exponentially
//     with jitter between attempts and logging loudly on every failure;
//   - on a successful re-auth it swaps the fresh token onto the SAME
//     *vaultapi.Client every consumer already holds and starts a new
//     watcher — see reauthWithBackoff's doc for why that swap is safe for
//     concurrent readers;
//   - a bare static token (VAULT_TOKEN) has no credential material to redo a
//     login with, so its Supervisor only renews (when Vault reports the
//     token renewable) or detects+alarms (when it does not) — see
//     startStaticTokenSupervisor.
//
// AuthState is the #751 task-4 health probe: nil while currently
// authenticated, the most recent failure otherwise. Exported so a readiness
// probe (tracked separately in #748) can consume it without depending on any
// of this package's internals beyond this one method.
type Supervisor struct {
	client *vaultapi.Client
	cfg    Config

	// reauthable is true only for AppRole/Kubernetes logins, which have
	// credential material login() can redo. A bare token has none — see
	// startStaticTokenSupervisor, which leaves this false (the zero value).
	reauthable bool

	mu  sync.Mutex
	err error

	// kick lets NotifyPermissionDenied end the CURRENT watch early — e.g. a
	// runtime 403 whose self-lookup confirms the token is already dead —
	// instead of waiting out its nominal remaining lease. Buffered(1):
	// coalesces a burst of concurrent notifications into a single wakeup.
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

// superviseLoop watches secret to the end of its life, then either
// re-authenticates (credential-backed methods) or settles into the terminal
// failed state (static tokens have nothing to redo a login with), repeating
// for as long as ctx lives.
func (s *Supervisor) superviseLoop(ctx context.Context, secret *vaultapi.Secret) {
	for {
		s.watchOnce(ctx, secret)
		if ctx.Err() != nil {
			return
		}
		if !s.reauthable {
			s.setErr(fmt.Errorf("vault: static token auth has no re-authentication path and its lease/renewal has ended — Vault access WILL fail until the operator issues a fresh token (vault.token / VAULT_TOKEN) and restarts the process"))
			log.Error("vault: static Vault token expired or was revoked with no automatic recovery possible — issue a new token and restart")
			return
		}
		newSecret, ok := s.reauthWithBackoff(ctx)
		if !ok {
			return
		}
		s.setErr(nil)
		secret = newSecret
	}
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

// reauthWithBackoff re-runs login with exponential backoff + jitter until it
// succeeds or ctx is cancelled, logging loudly (repeated ERROR, not once) on
// every failed attempt — the operator-facing signal #751 exists to create,
// replacing 15-minutes-later cache-driven surprise with an immediate one.
//
// On success it swaps the fresh token onto the SAME *vaultapi.Client every
// consumer already holds via client.SetToken. This is safe for concurrent
// readers: vaultapi.Client guards SetToken/Token with its own internal
// modifyLock (sync.RWMutex; hashicorp/vault/api client.go), so a swap can
// never race a concurrent Token() read into a torn value. A request already
// in flight read whatever token was current when IT built its request; any
// request issued after the swap picks up the fresh token. No consumer (the KV
// adapter, the Transit adapter, the capability prober) needs to be told the
// token changed or hold a lock of its own.
func (s *Supervisor) reauthWithBackoff(ctx context.Context) (*vaultapi.Secret, bool) {
	const (
		initialBackoff = 2 * time.Second
		maxBackoff     = 2 * time.Minute
	)
	backoff := initialBackoff
	for attempt := 1; ; attempt++ {
		secret, err := login(ctx, s.client, s.cfg)
		if err == nil && secret != nil && secret.Auth != nil && secret.Auth.ClientToken != "" {
			s.client.SetToken(secret.Auth.ClientToken)
			log.WithField("attempts", attempt).Info("vault: re-authentication succeeded")
			return secret, true
		}
		if err == nil {
			err = fmt.Errorf("vault: re-login returned no token")
		}
		s.setErr(fmt.Errorf("vault: re-authentication attempt %d failed: %w", attempt, err))
		log.WithError(err).WithField("attempt", attempt).
			Error("vault: RE-AUTHENTICATION FAILED — merchant secret / signing operations are degraded until this clears; retrying with backoff")

		wait := backoff/2 + time.Duration(rand.Int63n(int64(backoff/2)+1))
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// startStaticTokenSupervisor builds the Supervisor for token-mode auth (#751
// task 3). A bare token has no login material to redo, so its Supervisor
// never re-authenticates — it only renews (if Vault reports the token
// renewable) or detects and loudly alarms (if not):
//
//   - renewable: the SAME watch loop as approle/kubernetes keeps it alive
//     indefinitely on Vault's own renewal machinery. If renewal eventually
//     exhausts (Vault enforces a max TTL on periodic tokens too), there is
//     still nothing to re-authenticate WITH, so the terminal state is the
//     loud failure below.
//   - non-renewable with a real TTL: a prominent boot-time warning names the
//     exact expiry deadline — no silent decay. A background watch flips
//     AuthState() to a loud, permanent error right as the token's lease
//     ends, so a runtime 403 is never the first signal.
//   - non-renewable with TTL == 0 (Vault's shape for non-expiring root/dev
//     tokens): nothing to watch — these keep working silently, as before.
func startStaticTokenSupervisor(ctx context.Context, client *vaultapi.Client, token string) (*Supervisor, error) {
	sup := &Supervisor{client: client, kick: make(chan struct{}, 1)}

	lookup, err := client.Auth().Token().LookupSelfWithContext(ctx)
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

	switch {
	case renewable:
		secret := &vaultapi.Secret{Auth: &vaultapi.SecretAuth{ClientToken: token, Renewable: true, LeaseDuration: int(ttl.Seconds())}}
		go sup.superviseLoop(ctx, secret)
	case ttl > 0:
		deadline := time.Now().Add(ttl)
		log.Warnf("vault: token auth uses a NON-RENEWABLE token expiring at %s (in %s) — Vault access WILL stop then with NO automatic recovery; rotate vault.token/VAULT_TOKEN and restart the process before that deadline", deadline.Format(time.RFC3339), ttl.Round(time.Second))
		secret := &vaultapi.Secret{Auth: &vaultapi.SecretAuth{ClientToken: token, Renewable: false, LeaseDuration: int(ttl.Seconds())}}
		go sup.superviseLoop(ctx, secret)
	default:
		// Non-expiring root/dev token: nothing to watch; keeps working
		// silently forever, matching the documented contract.
	}
	return sup, nil
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
