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
// skipped and merchant isolation silently degrades to whatever explicit
// `WHERE merchant_id = ...` predicates each query happens to carry. This type makes
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

// EnforceRLSPosture FAILS startup if the connected role does not enforce RLS.
// It takes no environment: the requirement is UNCONDITIONAL, development
// included (or#782).
//
// Development used to be exempt, and that exemption was the defect factory. A
// component that reads an RLS-forced table without a merchant scope has its
// policy predicate degenerate to `merchant_id = NULL` and gets back zero rows
// with no error — so it logs success and does nothing. Under a BYPASSRLS role
// the same query returns rows, so the bug is invisible on the developer's
// machine and only inert in production (scheduled dunning that never found a
// due subscription, a webhook endpoint that was never registered, retention
// sweeps that never deleted). Dev has to run the production role or it keeps
// manufacturing that class.
//
// Migrations legitimately need the privileged role; they run through
// `openrails migrate`, which never builds the runtime and so never reaches here.
func (d *DB) EnforceRLSPosture(ctx context.Context) error {
	posture, err := d.CheckRLSPosture(ctx)
	if err != nil {
		return err
	}
	if err := rlsPostureError(posture); err != nil {
		return err
	}
	logrus.WithFields(logrus.Fields{
		"db_user":       posture.CurrentUser,
		"rls_enforcing": posture.Enforcing,
	}).Info("db: Row Level Security is ENFORCING for the connected role (issue #227)")
	return nil
}

// rlsPostureError returns a startup-fatal error when the connected role bypasses
// RLS. Pure (no DB) so the decision is unit testable independent of a live
// Postgres.
func rlsPostureError(posture RLSPosture) error {
	if posture.Enforcing {
		return nil
	}
	return fmt.Errorf(
		"db: role %q bypasses RLS (superuser/BYPASSRLS), so every merchant_isolation policy is skipped; "+
			"connect as the unprivileged openrails_app role (created by 0001_schema.up.sql) — this is required in "+
			"EVERY environment including development, and migrations are the only job that runs privileged",
		posture.CurrentUser,
	)
}
