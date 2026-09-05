package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// #692 action executor: a THIN composer over EXISTING machinery only — the
// lifecycle cancel chokepoint + the durable NMI-delete intent (queue-always
// #679, breaker-guarded), the admin refund producer (rail-intents ledger,
// effectively-once), the entitlement service's as-of revoke, and the
// product-access admin grant. It introduces NO new mutation logic.
//
// Semantics: idempotent per step (an already-cancelled subscription /
// already-revoked window / already-recorded grant no-ops); a partial failure
// returns the completed steps alongside the error so the caller can leave the
// finding OPEN with the compensation state documented.

// findingParamError marks operator-fixable input problems (bad/missing
// params, unsupported rail) — surfaced as 400 rather than 502.
type findingParamError struct{ msg string }

func (e *findingParamError) Error() string { return e.msg }

func paramErrorf(format string, args ...any) error {
	return &findingParamError{msg: fmt.Sprintf(format, args...)}
}

// findingActionErrorStatus maps executor errors onto HTTP statuses: operator
// input problems 400, refund producer statuses pass through, provider-side
// failures 502.
func findingActionErrorStatus(err error) int {
	var pe *findingParamError
	if errors.As(err, &pe) {
		return http.StatusBadRequest
	}
	var statusErr *adminRefundStatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status
	}
	return http.StatusBadGateway
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// paramAmountMicros parses a recommendation/override `amount` param into
// MICROS as an exact int64. or#863: this used to be `int64(raw.(float64))` —
// a float64 truncation on a value that feeds a real provider refund and an
// intents-ledger write. A float64 is now REJECTED rather than truncated
// (params are decoded with UseNumber, so an integer literal arrives as an
// exact json.Number); a decimal string is accepted for clients that prefer to
// keep money out of JSON numbers entirely.
func paramAmountMicros(raw any) (int64, error) {
	var text string
	switch v := raw.(type) {
	case json.Number:
		text = v.String()
	case string:
		text = strings.TrimSpace(v)
	case float64, float32:
		return 0, paramErrorf("recommendation param \"amount\" arrived as a floating-point value; " +
			"an amount in micros must be an exact integer (MONEY-3)")
	default:
		return 0, paramErrorf("recommendation param \"amount\" must be a positive integer number of micros")
	}
	micros, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, paramErrorf("recommendation param \"amount\" must be a positive integer number of micros (got %q)", text)
	}
	if micros <= 0 {
		return 0, paramErrorf("recommendation param \"amount\" must be a positive integer number of micros")
	}
	return micros, nil
}

func paramString(params map[string]any, key string) string {
	if v, ok := params[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func paramUUID(params map[string]any, key string) (uuid.UUID, error) {
	raw := paramString(params, key)
	if raw == "" {
		return uuid.Nil, paramErrorf("recommendation param %q is required", key)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, paramErrorf("recommendation param %q is not a valid uuid: %s", key, raw)
	}
	return id, nil
}

func paramOptionalUUID(params map[string]any, key string) (uuid.UUID, bool, error) {
	if params[key] == nil || paramString(params, key) == "" {
		return uuid.Nil, false, nil
	}
	id, err := paramUUID(params, key)
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// executeFindingAction dispatches one approved recommendation. The returned
// map documents every completed effect (recorded under evidence.resolution on
// success, appended to notes on partial failure).
func executeFindingAction(r *httprequest.Request, finding reconcile.FindingRecord, action string, params map[string]any, notes string) (map[string]any, error) {
	result := map[string]any{"action": action}
	switch action {
	case recommend.ActionAckResume:
		// Plain resolution: machinery keyed off the finding status (e.g. the
		// #679 breaker) re-arms once the finding leaves the open states.
		return result, nil
	case recommend.ActionRevokeEntitlement:
		return result, executeRevokeEntitlement(r, params, result)
	case recommend.ActionRecordAdminGrant:
		return result, executeRecordAdminGrant(r, finding, params, result)
	case recommend.ActionCancelAndRefund:
		return result, executeCancelAndRefund(r, finding, params, notes, result)
	default:
		return result, paramErrorf("unknown recommendation action %q", action)
	}
}

// executeRevokeEntitlement: as-of revoke via the existing entitlement service.
func executeRevokeEntitlement(r *httprequest.Request, params map[string]any, result map[string]any) error {
	svc := r.State.EntitlementService
	if svc == nil {
		return errors.New("entitlement service unavailable")
	}
	ctx := r.Request.Context()
	entID, err := paramUUID(params, "entitlement_id")
	if err != nil {
		return err
	}
	var asOf *time.Time
	if raw := paramString(params, "as_of"); raw != "" {
		parsed, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			return paramErrorf("recommendation param \"as_of\" must be RFC3339: %s", raw)
		}
		t := parsed.UTC()
		asOf = &t
	}
	ent, err := svc.GetByID(ctx, entID)
	if err != nil {
		return paramErrorf("entitlement %s not found", entID)
	}
	if ent.RevokedAt != nil {
		result["revoke"] = "noop_already_revoked"
		return nil
	}
	if err := svc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
		EntitlementID: &entID,
		Reason:        models.EntitlementRevokeAdmin,
		AsOf:          asOf,
	}); err != nil {
		return fmt.Errorf("revoke entitlement %s: %w", entID, err)
	}
	result["revoke"] = "revoked"
	result["entitlement_id"] = entID.String()
	convergeAfterMutation(r, ent.CustomerID) // #511: re-converge inline; the sweep re-measures
	return nil
}

// executeRecordAdminGrant: an admin-sourced product-access grant via the
// existing grants machinery (makes freeloader access legitimate). Idempotent:
// SourceID is keyed to the finding.
func executeRecordAdminGrant(r *httprequest.Request, finding reconcile.FindingRecord, params map[string]any, result map[string]any) error {
	svc := productAccessService(r)
	if svc == nil {
		return errors.New("product access service unavailable")
	}
	ctx := r.Request.Context()
	customerID := paramString(params, "customer_id")
	if customerID == "" {
		return paramErrorf("recommendation param \"customer_id\" is required")
	}
	productID, err := paramUUID(params, "product_id")
	if err != nil {
		return err
	}
	grant, created, err := svc.GrantProductAccess(ctx, productaccess.GrantParams{
		UserID:     customerID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourceAdmin,
		SourceID:   "finding:" + finding.ID.String(),
	})
	if err != nil {
		return fmt.Errorf("record admin grant: %w", err)
	}
	result["grant_id"] = grant.ID.String()
	result["grant_created"] = created
	if reason := paramString(params, "reason"); reason != "" {
		result["reason"] = reason
	}
	convergeAfterMutation(r, grant.CustomerID)
	return nil
}

// executeCancelAndRefund: cancel FIRST (idempotent), then refund. A refund
// failure after a successful cancel is a partial failure — the result map
// documents the completed cancel so the finding's notes carry the
// compensation state. Both ids are optional (#690: a pure one-off ownership
// duplicate has no subscription — refund-only), but at least one is required.
func executeCancelAndRefund(r *httprequest.Request, finding reconcile.FindingRecord, params map[string]any, notes string, result map[string]any) error {
	subID, hasCancel, err := paramOptionalUUID(params, "subscription_id")
	if err != nil {
		return err
	}
	paymentID, hasRefund, err := paramOptionalUUID(params, "refund_payment_id")
	if err != nil {
		return err
	}
	if !hasCancel && !hasRefund {
		return paramErrorf("cancel_and_refund needs at least one of \"subscription_id\", \"refund_payment_id\"")
	}
	reason := "operator findings queue approve (finding " + finding.ID.String() + ")"
	if notes != "" {
		reason += ": " + notes
	}

	// Reject unsupported refund semantics before the independent cancel leg can
	// change local access or enqueue a provider mutation.
	if hasRefund {
		if r.State.PaymentService == nil {
			return errors.New("payment service unavailable")
		}
		payment, err := r.State.PaymentService.GetByID(r.Request.Context(), paymentID)
		if err != nil {
			return paramErrorf("refund payment %s not found", paymentID)
		}
		if payment.Rail == models.RailCCBill {
			return paramErrorf("%s", ccbill.ErrRefundUnsupported)
		}
	}

	if hasCancel {
		if err := cancelSubscriptionForFinding(r, subID, reason, result); err != nil {
			return err
		}
	}
	if !hasRefund {
		return nil
	}
	return refundPaymentForFinding(r, finding, paymentID, params, reason, result)
}

// cancelSubscriptionForFinding cancels one LOCAL subscription through the
// lifecycle chokepoint; the remote side of NMI-backed rails rides the durable
// nmi_delete_subscription intent (queue-always #679 — the breaker gates its
// EXECUTION, never the enqueue). Mirrors the ResolveCancelledRemoteAlive
// pattern: cancellation UPDATE (with the marker) and intent enqueue commit in
// ONE transaction.
func cancelSubscriptionForFinding(r *httprequest.Request, subID uuid.UUID, reason string, result map[string]any) error {
	if r.State.SubscriptionService == nil || r.State.SubscriptionLifecycleService == nil {
		return errors.New("subscription services unavailable")
	}
	ctx := r.Request.Context()
	sub, err := r.State.SubscriptionService.GetByID(ctx, subID)
	if err != nil {
		if db.IsNotFound(err) {
			return paramErrorf("subscription %s not found", subID)
		}
		return fmt.Errorf("load subscription %s: %w", subID, err)
	}
	if sub.Status == models.StatusCancelled {
		result["cancel"] = "noop_already_cancelled"
		return nil
	}
	now := r.Clock.Now().UTC()
	feedback := reason

	switch {
	case rails.IsNMI(sub.Rail), sub.Rail == models.RailCCBill:
		localResult := make(map[string]any)
		err := r.State.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			txdb := r.State.DB.NewWithPgxTx(tx)
			sub, err := subscriptions.NewSubscriptionRepo(txdb).GetByIDForUpdate(ctx, subID)
			if err != nil {
				return fmt.Errorf("lock subscription %s: %w", subID, err)
			}
			if sub.Status == models.StatusCancelled {
				localResult["cancel"] = "noop_already_cancelled"
				return nil
			}
			if !rails.IsNMI(sub.Rail) && sub.Rail != models.RailCCBill {
				return fmt.Errorf("subscription %s rail changed before cancellation; retry", subID)
			}
			scheduleDelete := sub.RailSubscriptionID != ""
			if scheduleDelete && rails.IsNMI(sub.Rail) {
				sub.DeletionScheduledAt = &now
			}
			if err := r.State.SubscriptionLifecycleService.ApplyLocalCancellation(ctx, txdb, sub, subscriptions.LocalCancellation{
				EndedAt: now, CancelType: models.CancelTypeMerchant, Feedback: &feedback,
				RevokeReason: models.EntitlementRevokeAdmin, RevokeAsOf: now,
				RevokeSources: []models.EntitlementSourceType{models.EntitlementSourceSubscription, models.EntitlementSourceGrace},
			}); err != nil {
				return fmt.Errorf("cancel subscription %s: %w", subID, err)
			}
			if scheduleDelete {
				if rails.IsNMI(sub.Rail) {
					if err := intents.NewNMIDeleteScheduler(txdb, r.State.RateCeiling(), intents.OriginAdmin, reason).
						ScheduleNMIDelete(ctx, sub.CustomerID.String(), sub.ID, now); err != nil {
						return err
					}
					localResult["delete_intent"] = "queued"
				} else {
					if err := intents.NewCCBillCancelScheduler(txdb, r.State.RateCeiling(), intents.OriginAdmin, reason).
						ScheduleCCBillCancel(ctx, sub.CustomerID.String(), sub.ID); err != nil {
						return err
					}
					localResult["cancel_intent"] = "queued"
				}
			}
			localResult["cancel"] = "cancelled"
			localResult["subscription_id"] = subID.String()
			return nil
		})
		if err != nil {
			return err
		}
		for key, value := range localResult {
			result[key] = value
		}
		return nil
	case sub.Rail == models.RailStripe:
		stripeSvc := &subscriptions.StripeService{Config: r.State.Config, Rails: r.State.RailConfigs}
		if err := stripeSvc.CancelSubscription(ctx, sub.RailSubscriptionID); err != nil {
			return fmt.Errorf("cancel stripe subscription %s: %w", subID, err)
		}
		result["remote_cancel"] = "stripe_cancelled"
		if err := r.State.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: &sub.ID,
			CancelType:     models.CancelTypeMerchant,
			CancelFeedback: &feedback,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("stripe cancelled remotely but local cancel failed for %s: %w", subID, err)
		}
		result["cancel"] = "cancelled"
		result["subscription_id"] = subID.String()
		return nil
	default:
		// Solana (subscriber-signed by design) etc.: no operator-driven cancel
		// path here.
		return paramErrorf("cancel_and_refund is not supported for rail %q (subscription %s)", sub.Rail, subID)
	}
}

// refundPaymentForFinding executes the refund through the EXISTING admin
// refund producer: reservation + durable rail intent
// (effectively-once), recorded on the payments ledger by the handler's
// finalize — identical to POST /merchant/payments/{id}/refunds. The
// idempotency key is derived from the finding, so a re-approve retries the
// SAME refund instead of minting a second one.
func refundPaymentForFinding(r *httprequest.Request, finding reconcile.FindingRecord, paymentID uuid.UUID, params map[string]any, reason string, result map[string]any) error {
	if r.State.PaymentService == nil {
		return errors.New("payment service unavailable")
	}
	ctx := r.Request.Context()
	payment, err := r.State.PaymentService.GetByID(ctx, paymentID)
	if err != nil {
		return paramErrorf("refund payment %s not found", paymentID)
	}
	amount := payment.Amount // full refund by default (micros)
	if raw, ok := params["amount"]; ok && raw != nil {
		parsed, perr := paramAmountMicros(raw)
		if perr != nil {
			return perr
		}
		amount = parsed
	}
	idempotencyKey := "finding:" + finding.ID.String() + ":refund:" + paymentID.String()
	refund, status, err := executeAdminRefund(ctx, r, paymentID, refundRequest{
		Amount: amount,
		Reason: reason,
	}, idempotencyKey)
	if err != nil {
		return fmt.Errorf("refund payment %s: %w", paymentID, err)
	}
	result["refund_payment_id"] = paymentID.String()
	result["refund_amount"] = amount
	if refund != nil {
		result["refund_id"] = refund.ID.String()
	}
	// 201 = executed inline; 202 = queued on the durable intent ledger (the
	// scheduled executor/verifier owns completion — effectively-once).
	if status == http.StatusAccepted {
		result["refund"] = "queued"
	} else {
		result["refund"] = "completed"
	}
	return nil
}
