package service

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

// FindingPayPageUnreachable is a customer's page that could not frame an
// order's pay URL: the payment origin is down or misconfigured.
const FindingPayPageUnreachable reconcile.FindingType = "life.payment_page.unreachable"

// ReportPayURLFailure records that the customer's page could not frame their
// order's pay URL. One open finding per merchant collects the reports, and
// the operator is alerted once per episode.
func (s *Service) ReportPayURLFailure(ctx context.Context, actor OrderActor, id billing.OrderID) error {
	order, err := s.GetOrder(ctx, actor, id)
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
	log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid.String(), "order_id": order.ID.String(), "customer_id": order.CustomerID.String()}).
		Warn("payment page: the customer's page could not frame the order's pay URL")
	rec, err := (&reconcile.PGStore{DB: rt.DB}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingPayPageUnreachable, SubjectKey: "payment_page", Severity: reconcile.SeverityHigh,
		RecommendedAction: "Customers' pages could not load the payment page. Check the payment origin's DNS, TLS certificate and server.",
		Evidence:          map[string]any{"order_id": order.ID.String(), "customer_id": order.CustomerID.String(), "reported_at": s.now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return err
	}
	if rt.AlertService != nil {
		rt.Go("payment page alert", func(bg context.Context) {
			if err := rt.AlertService.NotifyFinding(merchant.WithID(bg, mid), rec); err != nil && bg.Err() == nil {
				log.WithError(err).Warn("payment page: alerting the operator failed; the finding stays in the console")
			}
		})
	}
	return nil
}
