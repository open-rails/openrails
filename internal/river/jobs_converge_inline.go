package riverjobs

import (
	"context"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// convergeCustomerInline runs the #511 Convergence Engine for one customer inline,
// right after a worker mutated that customer's state — the per-mutation companion
// to the 15-min ConvergeSweepWorker (Phase E). It establishes the merchant-scoped
// RLS connection, runs the idempotent Converge(customer) pass (DERIVE → LIFE →
// CON), and is BEST-EFFORT: a convergence error is only LOGGED, never returned, so
// it can never fail the mutation that already committed (the sweep is the backstop).
// A clean customer scope does zero writes, so the inline call is cheap.
//
// Use this from a worker whose merchant context is NOT already pinned. Inside a
// block already running under RunInMerchantConn, call converge.AfterMutation
// directly to avoid re-acquiring the connection.
func convergeCustomerInline(ctx context.Context, dbi *db.DB, merchantID, customer uuid.UUID, worker string) {
	if dbi == nil || merchantID == uuid.Nil || customer == uuid.Nil {
		return
	}
	mctx := merchant.WithID(ctx, merchant.ID(merchantID))
	if err := dbi.RunInMerchantConn(mctx, func(cctx context.Context) error {
		_, e := converge.AfterMutation(cctx, dbi, merchant.ID(merchantID), customer)
		return e
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"worker": worker, "merchant_id": merchantID, "customer_id": customer,
		}).Warn("inline converge after mutation failed; the sweep will reconcile")
	}
}
