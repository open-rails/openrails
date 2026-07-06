package embedded

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

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
func (e *Embedded) RunInMerchantConn(ctx context.Context, id merchant.ID, fn func(context.Context) error) error {
	if e == nil || e.app == nil || e.app.Runtime == nil || e.app.Runtime.DB == nil {
		return fmt.Errorf("embedded billing: not initialized")
	}
	return e.app.Runtime.DB.RunInMerchantConn(merchant.WithID(ctx, id), fn)
}
