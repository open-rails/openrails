package entitlements

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// RevokeRefundedPaymentAccess revokes the refunded payment's paid window and
// its renewal grace. Cancellation is a separate decision.
func (s *EntitlementService) RevokeRefundedPaymentAccess(ctx context.Context, paymentID uuid.UUID) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		payment, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: paymentID})
		if err != nil {
			return err
		}

		if err := lockOwner(ctx, q, mid.UUID(), payment.CustomerID); err != nil {
			return err
		}
		if _, err := q.LockPaymentSubscriptions(ctx, gen.LockPaymentSubscriptionsParams{MerchantID: mid.UUID(), PaymentID: paymentID}); err != nil {
			return err
		}
		if _, err := q.LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: mid.UUID(), PaymentID: paymentID}); err != nil {
			return err
		}
		review, err := q.RefundAccessNeedsReview(ctx, gen.RefundAccessNeedsReviewParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		if review {
			evidence, err := json.Marshal(struct {
				PaymentID  billing.PaymentID  `json:"payment_id"`
				CustomerID billing.CustomerID `json:"customer_id"`
				Reason     string             `json:"reason"`
			}{billing.PaymentID(paymentID), billing.CustomerID(payment.CustomerID), "payment access was combined or cannot be matched to its accepted period"})
			if err != nil {
				return err
			}
			action := "Refund recorded. Review this payment's access after the seat or plan change; unrelated paid access and renewal were preserved."
			_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
				MerchantID: mid.UUID(), FindingType: "consistency.refund_access", SubjectKey: billing.PaymentID(paymentID).String(), Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
			})
			return err
		}

		if err := q.MarkPaymentAccessRevoked(ctx, gen.MarkPaymentAccessRevokedParams{MerchantID: mid.UUID(), PaymentID: paymentID}); err != nil {
			return err
		}
		records, err := q.ListPaymentAccessGrants(ctx, gen.ListPaymentAccessGrantsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		ledger := s.ledger(q, mid.UUID())
		for _, g := range records {
			ended, err := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: mid.UUID(), GrantID: g.ID})
			if err != nil {
				return err
			}
			if !ended {
				if _, err := ledger.Revoke(ctx, g.ID, string(models.AccessRevokeRefund)); err != nil {
					return err
				}
			}
			if _, err := q.RevokeProductAccessByGrant(ctx, gen.RevokeProductAccessByGrantParams{MerchantID: mid.UUID(), GrantID: g.ID, RevokedAt: s.now().UTC(), RevokeReason: string(models.AccessRevokeRefund)}); err != nil {
				return err
			}
		}
		return nil
	})
}
