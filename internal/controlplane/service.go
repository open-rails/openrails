package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/config"
)

// ControlPlane is OpenRails' in-process AuthKit control plane (issue #224). It
// wraps an AuthKit http.Service (which exposes selectable route groups) and its
// underlying core.Service (used for in-process org/role/OAT bootstrap calls).
//
// It is constructed only when auth.control_plane.enabled is true. In pure
// verifier mode this is never built and OpenRails keeps acting as an external
// JWT verifier.
type ControlPlane struct {
	cfg     *config.Config
	authSvc *authhttp.Service
	pool    *pgxpool.Pool

	// delegatedVerifier validates browser-direct delegated access tokens for the
	// self-service surface (issue #222 browser tier). It is built from the same
	// issuer/audience/keys the control plane signs with, plus a self-service
	// permission-catalog validator. See delegated.go.
	delegatedVerifier *authhttp.Verifier
}

// New builds the OpenRails-owned AuthKit control plane from config and a pgx
// pool. The pool must point at the database that holds AuthKit's `profiles.*`
// schema (in self-hosted mode this is the same database OpenRails uses). The
// caller owns the pool lifecycle.
//
// Returns (nil, nil) when the control plane is disabled — callers should treat
// a nil ControlPlane as "verifier-only mode".
func New(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*ControlPlane, error) {
	if cfg == nil || cfg.Auth == nil || !cfg.Auth.ControlPlaneEnabled() {
		return nil, nil
	}
	if pool == nil {
		return nil, errors.New("controlplane: pgx pool is required when the control plane is enabled")
	}
	cp := cfg.Auth.ControlPlane

	issuer := strings.TrimSpace(cp.Issuer)
	if issuer == "" {
		return nil, errors.New("controlplane: auth.control_plane.issuer is required when enabled")
	}

	// Audiences default to the verifier's expected audience so issued/accepted
	// tokens line up with the existing verifier wiring when not overridden.
	issued := cp.IssuedAudiences
	expected := cp.ExpectedAudiences
	if len(issued) == 0 {
		if a := strings.TrimSpace(cfg.Auth.ExpectedAudience); a != "" {
			issued = []string{a}
		}
	}
	if len(expected) == 0 {
		if a := strings.TrimSpace(cfg.Auth.ExpectedAudience); a != "" {
			expected = []string{a}
		}
	}
	if len(issued) == 0 || len(expected) == 0 {
		return nil, errors.New("controlplane: issued_audiences/expected_audiences are required (or set auth.expected_audience)")
	}

	orgMode := strings.ToLower(strings.TrimSpace(cp.OrgMode))
	if orgMode == "" {
		// Operator-org bootstrap + org-management routes require multi mode.
		orgMode = "multi"
	}

	locked := cp.LockedDownEnabled()

	coreCfg := authcore.Config{
		Issuer:            issuer,
		BaseURL:           issuer,
		IssuedAudiences:   issued,
		ExpectedAudiences: expected,
		OrgMode:           orgMode,
		TokenPrefix:       strings.TrimSpace(cp.TokenPrefix),
		Environment:       strings.TrimSpace(cfg.Env),
		PermissionCatalog: Catalog(),
		DefaultRoles: []authcore.DefaultRole{
			// The operator role is also declared as a per-org DefaultRole so that
			// AssignRole can materialize it lazily even on orgs created outside the
			// bootstrap path. Bootstrap still defines+sets it explicitly.
			{Name: OperatorRole, Permissions: OperatorRolePermissions()},
		},
		// Self-hosted locked-down posture (#47 switches): no public user
		// self-registration, no public org onboarding/management. Embedded
		// bootstrap/core calls (CreateOrg/AssignRole/MintOAT) are unaffected.
		PublicRegistrationDisabled:  locked,
		PublicOrgManagementDisabled: locked,
	}

	authSvc, err := authhttp.NewService(coreCfg)
	if err != nil {
		return nil, fmt.Errorf("controlplane: build authkit service: %w", err)
	}
	authSvc = authSvc.WithPostgres(pool)

	// Build the browser-direct delegated-access-token verifier (#222 browser
	// tier). It accepts ONLY this control plane's own delegated tokens with the
	// canonical `openrails` audience and `openrails:self:*` permissions.
	delegatedVerifier, err := newDelegatedVerifier(authSvc.Core(), expected, orgMode, strings.TrimSpace(cp.TokenPrefix))
	if err != nil {
		return nil, fmt.Errorf("controlplane: build delegated verifier: %w", err)
	}

	return &ControlPlane{
		cfg:               cfg,
		authSvc:           authSvc,
		pool:              pool,
		delegatedVerifier: delegatedVerifier,
	}, nil
}

// Core returns the underlying AuthKit core service used for in-process
// org/role/OAT operations.
func (c *ControlPlane) Core() *authcore.Service {
	if c == nil {
		return nil
	}
	return c.authSvc.Core()
}

// AuthService returns the underlying AuthKit http.Service (for route mounting).
func (c *ControlPlane) AuthService() *authhttp.Service {
	if c == nil {
		return nil
	}
	return c.authSvc
}

// LockedDown reports whether this control plane runs in the locked-down,
// self-hosted posture.
func (c *ControlPlane) LockedDown() bool {
	if c == nil || c.cfg == nil || c.cfg.Auth == nil || c.cfg.Auth.ControlPlane == nil {
		return false
	}
	return c.cfg.Auth.ControlPlane.LockedDownEnabled()
}
