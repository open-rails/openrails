package service

import (
	"context"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

// FindingPayPageUnreachable is a customer's page that could not frame an
// invoice's pay URL: the checkout origin is down or misconfigured.
const FindingPayPageUnreachable reconcile.FindingType = "life.payment_page.unreachable"

// ReportPayURLFailure records that the customer's page could not frame their
// invoice's pay URL. One open finding per merchant collects the reports, and
// the operator is alerted once per episode.
func (s *Service) ReportPayURLFailure(ctx context.Context, actor InvoiceActor, id billing.InvoiceID) error {
	invoice, err := s.GetInvoice(ctx, actor, id)
	if err != nil {
		return err
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	s.Log.WarnContext(ctx, "payment page: the customer's page could not frame the invoice's pay URL", "merchant_id", mid.String(), "invoice_id", invoice.ID.String(), "customer_id", invoice.CustomerID.String())
	rec, err := (&reconcile.PGStore{DB: rt.DB}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingPayPageUnreachable, SubjectKey: "payment_page", Severity: reconcile.SeverityHigh,
		RecommendedAction: "Customers' pages could not load the payment page. Check the checkout origin's DNS, TLS certificate and server.",
		Evidence:          map[string]any{"invoice_id": invoice.ID.String(), "customer_id": invoice.CustomerID.String(), "reported_at": s.now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return err
	}
	if rt.AlertService != nil {
		rt.Go("payment page alert", func(bg context.Context) {
			if err := rt.AlertService.NotifyFinding(merchant.WithID(bg, mid), rec); err != nil && bg.Err() == nil {
				s.Log.WarnContext(ctx, "payment page: alerting the operator failed; the finding stays in the console", logx.Err(err))
			}
		})
	}
	return nil
}
