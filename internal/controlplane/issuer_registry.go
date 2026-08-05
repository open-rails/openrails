package controlplane

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

// DefaultIssuerRegistryTTL bounds how stale the delegated verifier's in-memory
// issuer registry may be against OUT-OF-BAND store writes (#852: the CLI
// `push-merchant-config` runs in a separate process, so no in-process reload
// sees its UpsertRemoteApplication). The verifier already lazy-loads brand-new
// issuers on first miss; this TTL additionally converges the cases a miss never
// triggers — rotated static keys / changed jwks_uri on an already-loaded
// issuer, and disable/revocation eviction. Refresh is activity-driven: a
// verification older than the TTL kicks ONE async re-sync; an idle server does
// no periodic work.
const DefaultIssuerRegistryTTL = time.Minute

// issuerRegistryRefreshTimeout caps one background re-sync (DB list + per-new-issuer
// JWKS fetches).
const issuerRegistryRefreshTimeout = 30 * time.Second

// issuerRegistryRefresh is the activity-driven TTL state for the delegated
// verifier's issuer registry. Zero value is ready to use.
type issuerRegistryRefresh struct {
	lastLoad atomic.Int64 // unix nanos of the last successful registry load
	ttl      atomic.Int64 // nanos; <=0 => DefaultIssuerRegistryTTL
	busy     atomic.Bool  // single-flights the background re-sync
}

// ErrDelegatedIssuerUnknown indicates a presented token's validated `iss` is not
// a registered AuthKit remote_application mapped via permission-group ownership to an
// active merchant. Fail closed: the token is rejected even if well-formed.
var ErrDelegatedIssuerUnknown = errors.New("controlplane: delegated token issuer maps to no active merchant")

// ErrRemoteApplicationSourceUnavailable indicates the control plane has no
// AuthKit core client to read remote_applications from (a partially-configured
// plane). Refreshes degrade to the existing in-memory registry.
var ErrRemoteApplicationSourceUnavailable = errors.New("controlplane: no remote_application source configured")

// loadRemoteApplications loads AuthKit's ACTIVE remote_applications into the
// delegated verifier (#481): standalone JWKS/issuer trust is AuthKit's
// remote_application registry (#74), NOT an OpenRails-owned table (the
// old delegated-issuer registry was dropped in #480). The verifier's
// in-house JWKS fetch/refresh handles keys; this is also re-callable to pick up
// store changes, and the verifier lazy-loads any single issuer on first use.
func (c *ControlPlane) loadRemoteApplications(ctx context.Context) error {
	if c == nil || c.delegatedVerifier == nil {
		return ErrDelegatedNotConfigured
	}
	// Core() is CONCRETE (*authcore.Client): a nil one boxed into the verifier's
	// RemoteApplicationSource interface is a non-nil interface, so authkit's own
	// `src == nil` fallback never fires and the load nil-derefs. The core client
	// IS the remote_application store — without it there is nothing to load.
	if c.Core() == nil {
		return ErrRemoteApplicationSourceUnavailable
	}
	if err := c.delegatedVerifier.LoadRemoteApplications(ctx, c.Core(), c.delegatedAudiences); err != nil {
		return err
	}
	c.issuerRefresh.lastLoad.Store(time.Now().UnixNano())
	return nil
}

// ReloadRemoteApplications re-syncs the verifier's in-memory issuer registry with
// AuthKit's remote_application store, picking up newly registered/disabled
// principals (the verifier also lazy-loads any single issuer on first use, so
// this is for deterministic reloads — e.g. after an inbound registration).
func (c *ControlPlane) ReloadRemoteApplications(ctx context.Context) error {
	return c.loadRemoteApplications(ctx)
}

// SetIssuerRegistryTTL overrides DefaultIssuerRegistryTTL (tests; <=0 restores
// the default).
func (c *ControlPlane) SetIssuerRegistryTTL(d time.Duration) {
	if c == nil {
		return
	}
	c.issuerRefresh.ttl.Store(int64(d))
}

// refreshIssuerRegistryIfStale kicks ONE async registry re-sync when the last
// successful load is older than the TTL (#852). Called from the delegated-
// verifier consumption points, so refresh work scales with verification
// traffic, never with wall clock. The current request still verifies against
// the existing registry (brand-new issuers are covered immediately by the
// verifier's own lazy-load-on-miss); convergence for updates/revocations is
// bounded by TTL + one reload.
func (c *ControlPlane) refreshIssuerRegistryIfStale() {
	// Core() nil => no remote_application store to re-sync from; don't spawn a
	// goroutine that can only fail (a partially-configured plane keeps verifying
	// against whatever registry it already holds).
	if c == nil || c.delegatedVerifier == nil || c.Core() == nil {
		return
	}
	ttl := time.Duration(c.issuerRefresh.ttl.Load())
	if ttl <= 0 {
		ttl = DefaultIssuerRegistryTTL
	}
	if time.Since(time.Unix(0, c.issuerRefresh.lastLoad.Load())) < ttl {
		return
	}
	if !c.issuerRefresh.busy.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.issuerRefresh.busy.Store(false)
		// A background timer must NEVER be able to kill the process: an
		// unrecovered panic here is not request-scoped, it tears down the whole
		// binary on a timer. Recover, log, and leave lastLoad unstamped so the
		// next verification retries — same degradation as a returned error.
		defer func() {
			if r := recover(); r != nil {
				log.WithFields(log.Fields{"panic": r, "stack": string(debug.Stack())}).
					Error("controlplane: issuer-registry TTL refresh panicked; registry left stale")
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), issuerRegistryRefreshTimeout)
		defer cancel()
		// A failure leaves lastLoad unstamped so the next verification retries;
		// verification meanwhile continues on the stale registry (fail-open on
		// refresh, fail-closed per token as before).
		if err := c.loadRemoteApplications(ctx); err != nil {
			log.WithError(err).Warn("controlplane: issuer-registry TTL refresh failed")
		}
	}()
}

// merchantForIssuer resolves the OpenRails MERCHANT a VALIDATED token issuer may
// act on (#567). The chain is group-based, never identity: validated `iss` ->
// AuthKit remote_application -> its controlling permission-group id (the merchant
// group) -> the merchant directory row whose recorded controlling group id
// matches (`merchants.permission_group_id`, repurposed under #567 to hold the merchant
// permission-group's internal id).
//
// #734: when a Host resolver has pinned a merchant onto ctx (merchant.WithHostMerchant
// — set only by the opt-in Host-based resolution middleware/mount, internal/http),
// the resolved issuer-merchant MUST equal it: a token minted for merchant A
// presented against merchant B's Host fails closed here, exactly like an
// unregistered issuer. No Host pin on ctx (the common case: no Host resolver
// configured) is a pure no-op — this is the SAME sentinel every other
// unresolvable-issuer case already returns, so every existing caller's error
// mapping covers it with no changes.
//
// Returns ErrDelegatedIssuerUnknown when the issuer is unregistered, is attached
// to no group, that group is no active merchant, or (#734) the resolved merchant
// disagrees with ctx's Host-pinned merchant (fail closed). The returned
// groupID/groupRef identify the merchant permission-group.
func (c *ControlPlane) merchantForIssuer(ctx context.Context, issuer string) (merchantID merchant.ID, merchantSlug, groupID, groupRef, remoteApplicationID string, err error) {
	issuer = strings.TrimSpace(issuer)
	if issuer == "" {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	if c == nil || c.Core() == nil || c.pool == nil {
		return merchant.ID{}, "", "", "", "", errors.New("controlplane: control plane unavailable for issuer resolution")
	}

	ra, err := c.Core().GetRemoteApplication(ctx, issuer)
	if err != nil || ra == nil || !ra.Enabled {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	// RemoteApplication.PermissionGroupID carries the controlling permission_group_id (#111):
	// the merchant group this issuer signs for. Empty => attached to nothing.
	groupID = strings.TrimSpace(ra.PermissionGroupID)
	if groupID == "" {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	mid, mslug, err := c.merchantDirectoryRow(ctx, `permission_group_id = $1`, groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	if err != nil {
		return merchant.ID{}, "", "", "", "", err
	}
	if hostMID, ok := merchant.HostMerchant(ctx); ok && hostMID != mid {
		return merchant.ID{}, "", "", "", "", ErrDelegatedIssuerUnknown
	}
	return mid, mslug, groupID, mslug, ra.ID, nil
}
