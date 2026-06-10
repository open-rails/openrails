package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/tenant"
)

// tenantPgxConnKey is the context key under which the request's tenant-scoped
// lazy connection is stored. Unexported so only this package manages the
// lifecycle.
type tenantPgxConnKey struct{}

// WithTenantConn arranges for the request to run on a single tenant-scoped
// connection: it puts a LAZY pinned connection in the context carrying the
// `app.tenant_id` session GUC (set on first use — see lazyTenantPgxConn).
// Every subsequent Qx(ctx)/Gen(ctx) on the returned context resolves to that
// connection, so all tenant-owned queries in the request are restricted to
// the request's tenant by the migration-050 RLS policies (fail-closed: a
// query on a connection without the GUC sees NO rows under the openrails_app
// role; issue #227).
//
// It pins a CONNECTION, not a transaction: no BEGIN, no locks held across
// slow work (e.g. a Stripe/NMI call between queries) — the connection simply
// sits idle. The returned release function MUST be called (defer) to reset
// the GUC and return the connection to the pool; resetting on release means a
// pooled connection never carries one tenant's GUC into another tenant's
// request.
//
// Returns the original ctx + a no-op release when this DB is tx-scoped
// (NewWithPgxTx) — the caller's tx carries the tenant GUC — or when a
// connection is already pinned (nested call).
func (d *DB) WithTenantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || (d.pool == nil && d.pgtx == nil) {
		return ctx, func() {}, fmt.Errorf("db: WithTenantConn on nil DB")
	}
	if d.pgtx != nil {
		return ctx, func() {}, nil
	}
	if _, ok := ctx.Value(tenantPgxConnKey{}).(*lazyTenantPgxConn); ok {
		return ctx, func() {}, nil
	}

	id := tenant.FromContextOrDefault(ctx)
	if id.IsZero() {
		return ctx, func() {}, fmt.Errorf("db: WithTenantConn requires a tenant in context")
	}

	lazy := &lazyTenantPgxConn{pool: d.pool, tenantID: id.String()}
	newCtx := context.WithValue(ctx, tenantPgxConnKey{}, lazy)
	return newCtx, lazy.release, nil
}

// RunInTenantConn pins a tenant connection for the duration of fn. It is the
// worker/background analogue of the request middleware: a River job (which has
// no HTTP request to pin a connection) wraps its tenant-owned work in
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
