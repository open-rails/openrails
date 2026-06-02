package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// TenantGUC is the Postgres run-time configuration parameter (GUC) that carries
// the active tenant id for Row Level Security. Migration 050 enables RLS on every
// tenant-owned table with a policy of the form
//
//	tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
//
// so a transaction that has set this GUC sees ONLY its own tenant's rows, and a
// transaction that forgot to set it sees NOTHING (fail-closed). For RLS to
// actually enforce, the application must connect as a non-superuser,
// non-BYPASSRLS role (openrails_app, created by migration 050).
const TenantGUC = "app.tenant_id"

// setTenantLocalGUC sets app.tenant_id for the remainder of the CURRENT
// transaction via set_config(..., is_local=true). It MUST run inside an open
// transaction: a transaction-local setting reverts when the tx ends, so it never
// leaks onto a pooled connection used by a later, different-tenant request.
//
// idb must be a transaction (bun.Tx) or a *bun.DB whose call is part of an
// enclosing tx; passing a raw *bun.DB makes the set_config its own one-statement
// transaction and the value is gone before the next query, which is why callers
// go through RunInTenantTx instead.
func setTenantLocalGUC(ctx context.Context, idb bun.IDB, id tenant.ID) error {
	if id.IsZero() {
		return fmt.Errorf("db: cannot set %s GUC for a zero tenant id", TenantGUC)
	}
	// set_config returns the new value; use a scalar query so it works uniformly
	// across bun.Tx and *bun.DB. Parameterised so the tenant id can never be
	// interpolated unsafely.
	var out string
	if err := idb.NewRaw(
		"SELECT set_config(?, ?, TRUE)", TenantGUC, id.String(),
	).Scan(ctx, &out); err != nil {
		return fmt.Errorf("db: set %s GUC: %w", TenantGUC, err)
	}
	return nil
}

// RunInTenantTx opens a transaction, pins the RLS tenant GUC from the context
// (tenant.FromContextOrDefault — so single-tenant / self-hosted runs as the
// default tenant), then runs fn within that transaction. Every tenant-owned
// query issued through the supplied tx is constrained to that tenant by the
// migration-050 RLS policies, regardless of whether the query itself includes a
// tenant_id predicate.
//
// This is the central, request-scoped entrypoint that plumbs the GUC: tenant-
// owned write/read paths that need RLS enforcement run through it (or call
// SetTenantGUC themselves when they already own a tx).
func (d *DB) RunInTenantTx(ctx context.Context, fn func(ctx context.Context, tx bun.Tx) error) error {
	if d == nil || d.db == nil {
		return fmt.Errorf("db: RunInTenantTx on nil DB")
	}
	id := tenant.FromContextOrDefault(ctx)
	switch dbi := d.db.(type) {
	case *bun.DB:
		return dbi.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if err := setTenantLocalGUC(ctx, tx, id); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	case bun.Tx:
		// Already inside a transaction: set the GUC on it and reuse it. The
		// setting is local to that enclosing tx.
		if err := setTenantLocalGUC(ctx, dbi, id); err != nil {
			return err
		}
		return fn(ctx, dbi)
	default:
		return fmt.Errorf("db: RunInTenantTx: unsupported db type %T", d.db)
	}
}

// SetTenantGUC pins the RLS tenant GUC (from the context) onto an already-open
// transaction. Use it from a callback that already received a bun.Tx (e.g. an
// existing RunInTx body) so that tx becomes RLS-scoped without restructuring the
// call site. It is a no-op-safe wrapper over set_config(..., is_local=true).
func SetTenantGUC(ctx context.Context, tx bun.Tx) error {
	return setTenantLocalGUC(ctx, tx, tenant.FromContextOrDefault(ctx))
}
