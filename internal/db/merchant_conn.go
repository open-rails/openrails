package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/merchant"
)

// merchantScopeKey is the context key of the request's merchant scope.
type merchantScopeKey struct{}

// merchantScope names the merchant every statement and transaction on pool
// in schema runs as. It holds no connection: each statement or transaction
// sets the merchant locally, so no session ever carries it past its
// transaction, through a transaction pooler or not.
type merchantScope struct {
	pool     *pgxpool.Pool
	tenantID string
	schema   string
}

// WithMerchantConn scopes Qx and Gen to the context's merchant: each
// statement, and each transaction begun on the pool, sets the merchant GUC
// locally. Queries still carry their merchant predicate. Slow work between
// queries holds no connection or transaction open. release is a no-op kept
// for callers that scope work around it.
func (d *DB) WithMerchantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || (d.pool == nil && d.pgtx == nil) {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn on nil DB")
	}
	if d.pgtx != nil {
		return transactionContext(ctx), func() {}, nil
	}
	if scope, err := d.merchantScope(ctx); err != nil {
		return ctx, func() {}, err
	} else if scope != nil {
		return ctx, func() {}, nil
	}
	id, err := merchant.Require(ctx)
	if err != nil {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn requires a merchant in context: %w", err)
	}
	if id.IsZero() {
		return ctx, func() {}, fmt.Errorf("db: WithMerchantConn requires a merchant in context")
	}
	scope := &merchantScope{pool: d.pool, tenantID: id.String(), schema: d.rw.schema()}
	return context.WithValue(ctx, merchantScopeKey{}, scope), func() {}, nil
}

// WithIndependentMerchantConn scopes a lease heartbeat's statements, which
// never join the request's open transaction: it refuses a tx-scoped DB.
func (d *DB) WithIndependentMerchantConn(ctx context.Context) (context.Context, func(), error) {
	if d == nil || d.pool == nil || d.pgtx != nil {
		return ctx, func() {}, fmt.Errorf("db: independent merchant connection requires a pool-backed DB")
	}
	return d.WithMerchantConn(ctx)
}

// DetachedWriteContext is for a write that records what already happened (a
// provider receipt, an intent outcome) after its caller may be gone. It drops
// the caller's cancellation, keeps every value and bounds the write by timeout.
func DetachedWriteContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}

// RunInMerchantConn scopes fn to the context's merchant: the background-job
// analogue of the request middleware.
func (d *DB) RunInMerchantConn(ctx context.Context, fn func(ctx context.Context) error) error {
	ctx, release, err := d.WithMerchantConn(ctx)
	if err != nil {
		return err
	}
	defer release()
	return fn(ctx)
}

// A request can visit independent databases. Reuse only this pool's scope;
// silently querying another database with the caller's scope changes authority.
func (d *DB) merchantScope(ctx context.Context) (*merchantScope, error) {
	scope, ok := ctx.Value(merchantScopeKey{}).(*merchantScope)
	if !ok || scope.pool != d.pool {
		return nil, nil
	}
	mid, ok := merchant.FromContext(ctx)
	if !ok || mid.String() != scope.tenantID || d.rw.schema() != scope.schema {
		return nil, fmt.Errorf("db: merchant scope does not match requested schema or merchant")
	}
	return scope, nil
}
