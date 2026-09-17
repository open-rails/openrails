package service

import (
	"context"
	"fmt"
)

// pin puts this call on a merchant-scoped connection for its whole duration, so
// every read it makes carries `app.merchant_id` and RLS answers with the
// merchant's rows instead of none.
//
// It is on EVERY exported method that reaches the database, not only the ones a
// host was told to wrap. There is no privileged pool (or#868): under the
// or#885-mandated `openrails_app` role an unpinned read of a policied table
// returns ZERO ROWS AND NO ERROR — "source price: no rows" about a price the
// caller had just written (or#900). Half this facade already pinned itself (the
// money surfaces, #227) and half relied on the caller, so a host could not tell
// from the outside which half it was talking to, and the half that did not pin
// failed by answering nothing rather than by failing.
//
// It nests, so there is nothing to weigh: on the HTTP path MerchantDBConnMW has
// already pinned and this is a no-op; a host that wraps a block of calls in
// embedded.RunInMerchant keeps ONE connection for the whole block; a River
// worker inherits its RunInMerchantScope pin. It pins a connection, not a
// transaction — no BEGIN, no locks held across a rail call.
//
// A caller with no merchant on the context gets a loud error here instead of an
// empty result, which is the whole point.
func (s *Service) pin(ctx context.Context) (context.Context, func(), error) {
	if s == nil || s.rt == nil || s.rt.DB == nil {
		return ctx, func() {}, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.DB.WithMerchantConn(ctx)
}
