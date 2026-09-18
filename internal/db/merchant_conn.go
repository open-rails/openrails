package db

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/pkg/merchant"
)

// merchantPgxConnKey is the context key under which the request's merchant-scoped
// lazy connection is stored. Unexported so only this package manages the
// lifecycle.
type merchantPgxConnKey struct{}

// WithMerchantConn arranges for the request to run on a single merchant-scoped
// connection: it puts a LAZY pinned connection in the context carrying the
// `app.merchant_id` session GUC (set on first use — see lazyMerchantPgxConn).
// Every subsequent Qx(ctx)/Gen(ctx) on the returned context resolves to that
// connection, so all merchant-owned queries in the request are restricted to
// the request's merchant by the migration-050 RLS policies (fail-closed: a
// query on a connection without the GUC sees NO rows under the openrails_app
// role; issue #227).
//
// It pins a CONNECTION, not a transaction: no BEGIN, no locks held across
// slow work (e.g. a Stripe/NMI call between queries) — the connection simply
// sits idle. The returned release function MUST be called (defer) to reset
// the GUC and return the connection to the pool; resetting on release means a
// pooled connection never carries one merchant's GUC into another merchant's
// request.
//
// Returns the original ctx + a no-op release when this DB is tx-scoped
// (NewWithPgxTx) — the caller's tx carries the merchant GUC — or when a
// connection is already pinned (nested call).
func (d *DB) WithMerchantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || (d.pool == nil && d.pgtx == nil) {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn on nil DB")
	}
	if d.pgtx != nil {
		return ctx, func() {}, nil
	}
	if lc, err := d.merchantConn(ctx); err != nil {
		return ctx, func() {}, err
	} else if lc != nil {
		return ctx, func() {}, nil
	}

	id, err := merchant.Require(ctx)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn requires a merchant in context: %w", err)
	}
	if id.IsZero() {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn requires a merchant in context")
	}

	lazy := &lazyMerchantPgxConn{pool: d.pool, tenantID: id.String(), schema: d.rw.schema()}
	newCtx := context.WithValue(ctx, merchantPgxConnKey{}, lazy)
	return newCtx, lazy.release, nil
}

// detachedWriteKey marks a context built by DetachedWriteContext.
type detachedWriteKey struct{}

// DetachedWriteContext is for a write that records what already happened — a
// provider receipt, an intent outcome — after its caller may be gone. It drops
// the caller's cancellation, keeps every value (merchant, PSP, the request's
// pinned connection) and bounds the write by timeout.
//
// The caller's cancellation can close the pinned connection: pgx closes a
// connection whose BEGIN or in-flight query is interrupted. A detached write
// then re-pins: the dead connection goes back to the pool (which destroys it,
// freeing its slot) before a fresh one is acquired and scoped to the same
// merchant, so a one-connection pool never waits on itself. A live pin is always
// reused. Ordinary request queries never re-pin; they keep failing on a dead
// connection rather than silently continuing on a new session.
func DetachedWriteContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithValue(context.WithoutCancel(ctx), detachedWriteKey{}, true), timeout)
}

func isDetachedWrite(ctx context.Context) bool {
	_, ok := ctx.Value(detachedWriteKey{}).(bool)
	return ok
}

// RunInMerchantConn pins a merchant connection for the duration of fn. It is the
// worker/background analogue of the request middleware: a River job (which has
// no HTTP request to pin a connection) wraps its merchant-owned work in
// RunInMerchantConn so its reads + writes run RLS-scoped under the openrails_app
// role, exactly like a request.
func (d *DB) RunInMerchantConn(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx, release, err := d.WithMerchantConn(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// A request can visit independent databases. Reuse only this pool's pin;
// silently querying another database with the caller's pin changes authority.
func (d *DB) merchantConn(ctx context.Context) (*lazyMerchantPgxConn, error) {
	lc, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn)
	if !ok || lc.pool != d.pool {
		return nil, nil
	}
	mid, ok := merchant.FromContext(ctx)
	if !ok || mid.String() != lc.tenantID || d.rw.schema() != lc.schema {
		return nil, fmt.Errorf("db: merchant connection does not match requested schema or merchant")
	}
	return lc, nil
}
