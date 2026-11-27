package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/payment"
)

// KindInvoiceExpiry is the billing pass's invoice expiry scan.
const KindInvoiceExpiry = "openrails.invoice_expiry"

// InvoiceExpiryWorker expires customer purchases past their expiry, the only
// invoices that have one, releasing their claims, and the invoice attempts
// still waiting on their customer past theirs: it is their one expiry writer. It never touches an invoice whose payment is
// processing: only the provider's answer ends that. It also removes card
// saves left waiting for the bank's authentication.
type InvoiceExpiryWorker struct {
	DB       *db.DB
	Invoices *invoices.Service
	Clock    clockwork.Clock
}

// invoiceExpiryBatch is how many merchants, invoices or attempts one read takes.
// Each read is indexed on the expiry, so a pass costs what is due.
const invoiceExpiryBatch = 200

// DueBy reports an invoice or an invoice attempt past its expiry since before at.
func (w InvoiceExpiryWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := invoices.ExpiryMerchants(ctx, w.DB, at, uuid.Nil, 1)
	return len(due) > 0, err
}

// Pass pages through the merchants with something due and drains each, within
// the pass's budget; what the budget leaves stays due for the next pass.
func (w InvoiceExpiryWorker) Pass(ctx context.Context) error {
	if w.Invoices == nil {
		return nil
	}
	now := workerNow(w.Clock)
	// One merchant's failure never stops the others; the job reports them all.
	var workErr error
	for after := uuid.Nil; ; {
		merchants, err := invoices.ExpiryMerchants(ctx, w.DB, now, after, invoiceExpiryBatch)
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("invoice expiry: list merchants: %w", err))
		}
		for _, id := range merchants {
			if err := ctx.Err(); err != nil {
				return errors.Join(workErr, fmt.Errorf("invoice expiry: %w", err))
			}
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "invoice expiry sweep", func(ctx context.Context) error {
				if _, err := w.Invoices.ExpireDue(ctx, invoiceExpiryBatch); err != nil {
					return err
				}
				for {
					n, err := w.DB.Gen(ctx).ExpireInvoiceAttempts(ctx, gen.ExpireInvoiceAttemptsParams{MerchantID: id, Now: now, RowLimit: invoiceExpiryBatch})
					if err != nil || n < invoiceExpiryBatch {
						return err
					}
					if err := ctx.Err(); err != nil {
						return err
					}
				}
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("invoice expiry: merchant %s: %w", id, err))
			}
		}
		if len(merchants) < invoiceExpiryBatch {
			break
		}
		after = merchants[len(merchants)-1]
	}
	before := now.Add(-payment.SetupAbandonAfter)
	for after := uuid.Nil; ; {
		stale, err := w.DB.GenDirectory().ListStaleSetupMerchants(ctx, gen.ListStaleSetupMerchantsParams{Before: before, Now: now, AfterMerchantID: after, MerchantLimit: invoiceExpiryBatch})
		if err != nil {
			return errors.Join(workErr, fmt.Errorf("card save expiry: list merchants: %w", err))
		}
		for _, id := range stale {
			if err := ctx.Err(); err != nil {
				return errors.Join(workErr, fmt.Errorf("card save expiry: %w", err))
			}
			if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "card save expiry", func(ctx context.Context) error {
				_, err := w.DB.Gen(ctx).AbandonStalePaymentMethodSetups(ctx, gen.AbandonStalePaymentMethodSetupsParams{MerchantID: id, Before: before, Now: now})
				return err
			}); err != nil {
				workErr = errors.Join(workErr, fmt.Errorf("card save expiry: merchant %s: %w", id, err))
			}
		}
		if len(stale) < invoiceExpiryBatch {
			return workErr
		}
		after = stale[len(stale)-1]
	}
}
