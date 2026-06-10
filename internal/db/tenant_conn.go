package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// tenantConnKey is the context key under which a request-pinned, tenant-scoped
// bun.Conn is stored. Unexported so only this package manages the lifecycle.
type tenantConnKey struct{}

// tenantPgxConnKey is the pgx-side twin: the request-pinned *pgxpool.Conn
// carrying the same app.tenant_id GUC. During the bun -> sqlc transition
// (#334) WithTenantConn pins BOTH, so converted (sqlc) and unconverted (bun)
// call sites within one request are each RLS-scoped.
type tenantPgxConnKey struct{}

// Q returns the queryable handle that tenant-owned reads and writes MUST use so
// the migration-050 Row Level Security policies actually constrain them (issue
// #227).
//
// When a request (or worker block) has pinned a tenant-scoped connection via
// WithTenantConn, Q returns THAT connection — it has `app.tenant_id` set as a
// session GUC, so every query on it is restricted to the request's tenant by RLS
// (fail-closed: a query on a connection without the GUC sees NO rows under the
// openrails_app role). When no tenant connection is pinned (e.g. control-plane
// access to GLOBAL, non-RLS tables, or single-tenant self-hosted before the
// connection middleware runs), Q falls back to the base pool.
//
// This is the drop-in replacement for GetDB() at every tenant-owned call site.
func (d *DB) Q(ctx context.Context) bun.IDB {
	if d == nil {
		return nil
	}
	if _, ok := d.db.(bun.Tx); ok {
		return d.db
	}
	if c, ok := ctx.Value(tenantConnKey{}).(bun.Conn); ok {
		return c
	}
	return d.db
}

// WithTenantConn pins a single database connection for the supplied context and
// sets the `app.tenant_id` session GUC on it from the context's tenant
// (tenant.FromContextOrDefault — so single-tenant/self-hosted runs as the default
// tenant). Every subsequent Q(ctx) on the returned context returns this same
// connection, so all tenant-owned queries in the request share one RLS-scoped
// connection.
//
// It pins a CONNECTION, not a transaction: no BEGIN, no locks held across slow
// work (e.g. a Stripe/NMI call between queries) — the connection simply sits
// idle. The returned release function MUST be called (defer) to reset the GUC and
// return the connection to the pool; resetting on release means a pooled
// connection never carries one tenant's GUC into another tenant's request.
//
// Returns the original ctx + a no-op release when the underlying handle is not a
// *bun.DB (e.g. already a tx), so callers can use it unconditionally.
func (d *DB) WithTenantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || d.db == nil {
		return ctx, func() {}, fmt.Errorf("db: WithTenantConn on nil DB")
	}
	base, ok := d.db.(*bun.DB)
	if !ok {
		// Already a tx (or unsupported handle): nothing to pin.
		return ctx, func() {}, nil
	}
	// If a connection is already pinned (nested call), reuse it.
	if _, ok := ctx.Value(tenantConnKey{}).(bun.Conn); ok {
		return ctx, func() {}, nil
	}

	conn, err := base.Conn(ctx)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("db: acquire tenant connection: %w", err)
	}

	id := tenant.FromContextOrDefault(ctx)
	if id.IsZero() {
		_ = conn.Close()
		return ctx, func() {}, fmt.Errorf("db: WithTenantConn requires a tenant in context")
	}
	// is_local := false -> session-scoped on this pinned connection (persists
	// across the queries in this request, reset on release).
	var out string
	if err := conn.NewRaw("SELECT set_config(?, ?, FALSE)", TenantGUC, id.String()).
		Scan(ctx, &out); err != nil {
		_ = conn.Close()
		return ctx, func() {}, fmt.Errorf("db: set %s on tenant connection: %w", TenantGUC, err)
	}

	newCtx := context.WithValue(ctx, tenantConnKey{}, conn)

	// pgx-side twin (#334): pin a pool connection with the same session GUC so
	// sqlc-converted call sites in this request are RLS-scoped too.
	var pgxRelease func()
	if d.pool != nil {
		pgxConn, err := d.pool.Acquire(ctx)
		if err != nil {
			_ = conn.Close()
			return ctx, func() {}, fmt.Errorf("db: acquire pgx tenant connection: %w", err)
		}
		if err := pgxConn.QueryRow(ctx,
			"SELECT set_config($1, $2, FALSE)", TenantGUC, id.String()).Scan(&out); err != nil {
			pgxConn.Release()
			_ = conn.Close()
			return ctx, func() {}, fmt.Errorf("db: set %s on pgx tenant connection: %w", TenantGUC, err)
		}
		newCtx = context.WithValue(newCtx, tenantPgxConnKey{}, pgxConn)
		pgxRelease = func() {
			// Reset the GUC so the connection returns to the pool clean; works
			// even after the request context is cancelled.
			_, _ = pgxConn.Exec(context.Background(),
				"SELECT set_config($1, '', FALSE)", TenantGUC)
			pgxConn.Release()
		}
	}

	release := func() {
		// Reset the GUC so the connection returns to the pool clean, then close
		// (Close returns it to the pool). Use a background context so release works
		// even after the request context is cancelled.
		_, _ = conn.ExecContext(context.Background(), "SELECT set_config('"+TenantGUC+"', '', FALSE)")
		_ = conn.Close()
		if pgxRelease != nil {
			pgxRelease()
		}
	}
	return newCtx, release, nil
}

// BeginTenantTx begins a transaction with the RLS tenant GUC already set from the
// context (#227) — a drop-in replacement for `GetDB().(*bun.DB).BeginTx(ctx, nil)`
// on REQUEST-PATH code so the resulting tx is RLS-scoped under the openrails_app
// role. (Cross-tenant background workers must NOT use this: they run as a
// privileged role and sweep all tenants with explicit tenant_id predicates.)
func (d *DB) BeginTenantTx(ctx context.Context) (bun.Tx, error) {
	if d == nil || d.db == nil {
		return bun.Tx{}, fmt.Errorf("db: BeginTenantTx on nil DB")
	}
	bunDB, ok := d.db.(*bun.DB)
	if !ok {
		return bun.Tx{}, fmt.Errorf("db: BeginTenantTx requires *bun.DB (got %T)", d.db)
	}
	tx, err := bunDB.BeginTx(ctx, nil)
	if err != nil {
		return tx, err
	}
	if err := setTenantLocalGUC(ctx, tx, tenant.FromContextOrDefault(ctx)); err != nil {
		_ = tx.Rollback()
		return bun.Tx{}, err
	}
	return tx, nil
}

// RunInTenantConn pins a tenant connection for the duration of fn. It is the
// worker/background analogue of the request middleware: a River job (which has no
// HTTP request to pin a connection) wraps its tenant-owned work in
// RunInTenantConn so its reads + writes run RLS-scoped under the openrails_app
// role, exactly like a request.
func (d *DB) RunInTenantConn(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx, release, err := d.WithTenantConn(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}
