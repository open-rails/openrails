package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// ErrCallerTransaction refuses durable custody that cannot join a caller's
// rollback domain. A caller must initialize that custody before its domain tx.
var ErrCallerTransaction = errors.New("db: durable merchant custody requires an independent commit outside the caller transaction")

type transactionContextKey struct{}

func transactionContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, transactionContextKey{}, true)
}

// InCallerTransaction reports that ctx belongs to a transaction this package
// opened: work that must commit on its own runs before it.
func InCallerTransaction(ctx context.Context) bool {
	marked, _ := ctx.Value(transactionContextKey{}).(bool)
	return marked
}

// CommittedMerchantTx is for durable custody and queue handoff. It commits
// independently on a pooled connection of its own. It never joins or commits
// an existing caller-owned transaction. Supported DB/Pool transaction
// callbacks carry the private marker below; manually managed raw pgx
// transactions must use BindMerchantTx to bind context.
func (p *Pool) CommittedMerchantTx(ctx context.Context, id billing.MerchantID, fn func(context.Context, pgx.Tx) error) error {
	if p == nil || p.raw == nil || id.IsZero() {
		return fmt.Errorf("db: committed merchant transaction requires pool and merchant")
	}
	if InCallerTransaction(ctx) {
		return ErrCallerTransaction
	}
	if current, ok := merchant.FromContext(ctx); ok && current != id {
		return fmt.Errorf("db: committed merchant transaction has a different merchant scope")
	}
	if scope, ok := ctx.Value(merchantScopeKey{}).(*merchantScope); ok &&
		(scope.pool != p.raw || scope.tenantID != id.String() || scope.schema != p.rw.schema()) {
		return fmt.Errorf("db: committed merchant transaction has an incompatible merchant scope")
	}
	conn, err := acquire(ctx, p.raw)
	if err != nil {
		return err
	}
	defer conn.Release()
	raw, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	tx := p.rw.wrapTx(raw)
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err = setMerchantLocalGUCPgx(ctx, tx, id); err != nil {
		return err
	}
	if err = fn(transactionContext(merchant.WithID(ctx, id)), tx); err != nil {
		return err
	}
	return commit(ctx, tx)
}
