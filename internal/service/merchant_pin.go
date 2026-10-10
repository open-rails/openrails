package service

import (
	"context"
	"fmt"
)

// pin puts this call on one merchant-scoped connection carrying
// `openrails.merchant_id`. Every exported method that reaches the database
// calls it, so no read depends on the caller having pinned.
//
// It nests: under MerchantDBConnMW or a worker's RunInMerchantScope it is a
// no-op. It pins a connection, not a transaction: no BEGIN, no locks held
// across a rail call. A context with no merchant is an error, never an empty
// result.
func (s *Service) pin(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.rt == nil || s.rt.DB == nil {
		return ctx, func() {}, fmt.Errorf("billing service: not initialized")
	}
	if s.catalogTx != nil {
		return s.catalogTx.WithMerchantConn(ctx)
	}
	return s.rt.DB.WithMerchantConn(ctx)
}
