package db

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/merchant"
)

// merchantPgxConnKey is the context key of the request's lazy merchant connection.
type merchantPgxConnKey struct{}

// WithMerchantConn puts a lazy connection pinned to the context's merchant
// (the openrails.merchant_id session GUC, set on first use) in the context;
// Qx/Gen on the returned context use it. The GUC filters nothing by itself:
// tenant queries still carry their merchant predicate.
//
// It pins a connection, not a transaction, so slow work between queries holds
// no locks. The caller MUST call release, which resets the GUC so a pooled
// connection never carries one merchant into another's request. A tx-scoped DB,
// or an already compatible pin, returns a no-op release.
func (d *DB) WithMerchantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || (d.pool == nil && d.pgtx == nil) {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn on nil DB")
	}
	if d.pgtx != nil {
		return transactionContext(ctx), func() {}, nil
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

// WithIndependentMerchantConn gives a lease heartbeat its own lazy pin on this
// database. It preserves cancellation and context values, but cannot join the
// request's open transaction. Acquisition may wait for pool capacity; canceling
// the heartbeat cancels that wait without closing the request's connection.
func (d *DB) WithIndependentMerchantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || d.pool == nil || d.pgtx != nil {
		return ctx, func() {}, fmt.Errorf("db: independent merchant connection requires a pool-backed DB")
	}
	id, err := merchant.Require(ctx)
	if err != nil {
		return ctx, func() {}, err
	}
	if inherited, ok := ctx.Value(merchantPgxConnKey{}).(*lazyMerchantPgxConn); ok &&
		(inherited.pool != d.pool || inherited.tenantID != id.String() || inherited.schema != d.rw.schema()) {
		return ctx, func() {}, fmt.Errorf("db: inherited merchant connection does not match database, schema or merchant")
	}
	lazy := &lazyMerchantPgxConn{pool: d.pool, tenantID: id.String(), schema: d.rw.schema()}
	return context.WithValue(ctx, merchantPgxConnKey{}, lazy), lazy.release, nil
}

// detachedWriteKey marks a context built by DetachedWriteContext.
type detachedWriteKey struct{}

// DetachedWriteContext is for a write that records what already happened (a
// provider receipt, an intent outcome) after its caller may be gone. It drops
// the caller's cancellation, keeps every value and bounds the write by timeout.
//
// The caller's cancellation can close the pinned connection. A detached write
// then re-pins, releasing the dead connection first so a one-connection pool
// never waits on itself. Ordinary queries never re-pin: they fail on a dead
// connection rather than continue on a new session.
func DetachedWriteContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithValue(context.WithoutCancel(ctx), detachedWriteKey{}, true), timeout)
}

func isDetachedWrite(ctx context.Context) bool {
	_, ok := ctx.Value(detachedWriteKey{}).(bool)
	return ok
}

// RunInMerchantConn pins a merchant connection for the duration of fn: the
// background-job analogue of the request middleware.
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
