package embedded

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// RunInMerchant is the no-argument pin: it scopes fn to the merchant THIS
// engine is bound to (the same binding the HTTP surface pins per request via
// middleware.ResolveMerchantHTTP), so an in-process Go caller of
// *service.Service gets the automatic pin an HTTP caller already had (#814).
// There is no id to pass and therefore no id to get wrong — the mispin that
// reads another merchant's rows is not expressible.
//
// An engine that is not bound to a merchant (a multi-merchant standalone
// deployment) gets a loud error, never a default merchant: name the merchant
// the unit of work belongs to via RunInMerchantConn instead.
func (e *Embedded) RunInMerchant(ctx context.Context, fn func(context.Context) error) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("embedded billing: not initialized")
	}
	id := e.app.Runtime.ConfiguredMerchant()
	if id.IsZero() {
		return fmt.Errorf("embedded billing: RunInMerchant requires an engine bound to a merchant (embed.UpsertMerchantConfig binds it); a multi-merchant engine must name the merchant via RunInMerchantConn")
	}
	return e.RunInMerchantConn(ctx, id, fn)
}

// RunInMerchantConn pins ctx to a merchant-scoped DB connection for the
// duration of fn — the exported analogue of internal/db.DB.RunInMerchantConn
// for a host calling *service.Service (or any other merchant-owned engine Go
// API) DIRECTLY, outside of an HTTP request (which gets this for free via
// middleware.ResolveMerchantHTTP) or a River job (which gets it via the
// worker wiring). Every merchant-owned table's RLS policy requires the
// `app.merchant_id` GUC this pins (issue #227): a caller that skips it does
// not fail gracefully — reads silently see zero rows and writes are rejected
// by the table's RLS policy, for EVERY merchant, indistinguishable from a
// generic permission error. A host doing boot-time catalog seeding or other
// one-off Go-native merchant work (as opposed to a bulk manifest operation
// already covered by ConvergeMerchant/PushMerchantCatalog/dump) needs this.
//
// The pin is PROVEN before fn runs (or#868's RunInMerchantScope): one cheap
// round trip reads app.merchant_id back, so a pin that did not take fails
// loudly instead of silently processing nothing. And when the engine is bound
// to a single merchant, an id naming a DIFFERENT merchant is refused (#814) —
// on a single-merchant engine that is always a host bug, and the rows it would
// touch are somebody else's.
func (e *Embedded) RunInMerchantConn(ctx context.Context, id merchant.ID, fn func(context.Context) error) error {
	if e == nil || e.app == nil || e.app.Runtime == nil || e.app.Runtime.DB == nil {
		return fmt.Errorf("embedded billing: not initialized")
	}
	if bound := e.app.Runtime.ConfiguredMerchant(); !bound.IsZero() && !id.IsZero() && bound != id {
		return fmt.Errorf("embedded billing: this engine is bound to merchant %s but the call names merchant %s — a single-merchant engine never reaches another merchant's rows (#814)", bound, id)
	}
	return e.app.Runtime.DB.RunInMerchantScope(ctx, id, "embedded merchant work", fn)
}
