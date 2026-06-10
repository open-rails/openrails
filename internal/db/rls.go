package db

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
)

// RLSPosture describes whether the role the application is currently connected
// as actually ENFORCES the migration-050 Row Level Security policies.
//
// RLS only constrains a connection that is NEITHER a superuser NOR a BYPASSRLS
// role. The migration enables + FORCEs RLS and creates the unprivileged
// `openrails_app` role, but enforcement is a property of the CONNECTED role: if
// the app connects as a superuser (or any BYPASSRLS role), every policy is
// skipped and tenant isolation silently degrades to whatever explicit
// `WHERE tenant_id = ...` predicates each query happens to carry. This type makes
// that posture observable instead of a silent footgun.
type RLSPosture struct {
	// CurrentUser is the Postgres role the connection authenticated as.
	CurrentUser string
	// Enforcing reports whether RLS policies actually apply to this role
	// (true => not a superuser and not BYPASSRLS).
	Enforcing bool
}

// CheckRLSPosture reports whether the current connection's role enforces RLS. A
// role enforces RLS when it is neither a superuser nor a BYPASSRLS role.
func (d *DB) CheckRLSPosture(ctx context.Context) (RLSPosture, error) {
	if d == nil || d.pool == nil {
		return RLSPosture{}, fmt.Errorf("db: CheckRLSPosture on nil DB")
	}
	var (
		user   string
		bypass bool
	)
	// rolsuper OR rolbypassrls captures both ways a role skips RLS. current_user
	// is the effective role for the session.
	err := d.pool.QueryRow(ctx, `
		SELECT current_user,
		       COALESCE(bool_or(rolsuper OR rolbypassrls), TRUE)
		  FROM pg_roles
		 WHERE rolname = current_user
	`).Scan(&user, &bypass)
	if err != nil {
		return RLSPosture{}, fmt.Errorf("db: read RLS posture: %w", err)
	}
	return RLSPosture{CurrentUser: user, Enforcing: !bypass}, nil
}

// EnforceRLSPosture logs the connection's RLS posture and, when require is true
// (managed multi-tenant deployments), FAILS startup if the connected role does
// not enforce RLS. This is the guard that prevents shipping a managed/hosted
// OpenRails that believes it has tenant isolation while actually connecting as a
// privileged role that bypasses every policy (issue #227).
//
// For self-hosted single-tenant deployments require is false: RLS is still a
// useful backstop but running as a privileged role is acceptable because there is
// only one tenant.
func (d *DB) EnforceRLSPosture(ctx context.Context, require bool) error {
	posture, err := d.CheckRLSPosture(ctx)
	if err != nil {
		return err
	}
	fields := logrus.Fields{
		"db_user":       posture.CurrentUser,
		"rls_enforcing": posture.Enforcing,
		"rls_required":  require,
	}
	if posture.Enforcing {
		logrus.WithFields(fields).Info("db: Row Level Security is ENFORCING for the connected role (issue #227)")
		return nil
	}
	if err := rlsPostureError(posture, require); err != nil {
		return err
	}
	logrus.WithFields(fields).Warn(
		"db: connected role bypasses Row Level Security; tenant isolation relies on explicit query predicates only " +
			"(acceptable for self-hosted single-tenant; set the RLS-required flag + connect as openrails_app for managed multi-tenant)")
	return nil
}

// rlsPostureError returns a startup-fatal error when RLS enforcement is required
// but the connected role bypasses RLS. Pure (no DB) so the decision is unit
// testable independent of a live Postgres.
func rlsPostureError(posture RLSPosture, require bool) error {
	if posture.Enforcing || !require {
		return nil
	}
	return fmt.Errorf(
		"db: RLS enforcement required but role %q bypasses RLS (superuser/BYPASSRLS); "+
			"connect as the unprivileged openrails_app role (created by 001_schema.up.sql) in managed multi-tenant mode",
		posture.CurrentUser,
	)
}
