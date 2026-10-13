package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/destructive"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
)

// The finding action executor composes existing machinery only: the lifecycle
// cancel chokepoint with its durable provider-cancel intent (breaker-gated),
// the admin refund producer, the entitlement service's as-of revoke and the
// product-access grant. Each step is idempotent; a partial failure returns
// the completed steps with the error, so the finding stays open with its
// compensation state.

// findingParamError marks operator-fixable input problems (bad/missing
// params, unsupported rail).
type findingParamError struct{ msg string }

func (e *findingParamError) Error() string { return e.msg }

func paramErrorf(format string, args ...any) error {
	return &findingParamError{msg: fmt.Sprintf(format, args...)}
}

// findingActionRefusal answers a failed approve: operator input problems are
// invalid_param, a refused refund keeps its code, anything else is
// finding_action_failed. The finding stays open in every case.
func findingActionRefusal(err error) *api.APIError {
	message := "recommendation execution failed; finding remains open: " + err.Error()
	var param *findingParamError
	if errors.As(err, &param) {
		return api.Coded(billing.CodeInvalidParam, message)
	}
	if refusal := refusalOf(err); refusal != nil {
		out := *refusal
		out.Message = message
		return &out
	}
	var refusal *api.APIError
	if errors.As(err, &refusal) {
		out := *refusal
		out.Message = message
		return &out
	}
	return api.Coded(codeFindingActionFailed, message)
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// paramAmountMicros parses an `amount` param as exact int64 micros: a
// json.Number integer (params decode with UseNumber) or a decimal string. A
// float64 is refused, never truncated.
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

// paramTypedID reads a typed id param in its wire spelling; the bare UUID is
// refused like everywhere else on the wire.
func paramTypedID[T interface{ IsZero() bool }](params map[string]any, key string, parse func(string) (T, error)) (T, error) {
	var zero T
	raw := paramString(params, key)
	if raw == "" {
		return zero, paramErrorf("recommendation param %q is required", key)
	}
	id, err := parse(raw)
	if err != nil || id.IsZero() {
		return zero, paramErrorf("recommendation param %q is not a valid id: %s", key, raw)
	}
	return id, nil
}

func paramOptionalTypedID[T interface{ IsZero() bool }](params map[string]any, key string, parse func(string) (T, error)) (T, bool, error) {
	var zero T
	if params[key] == nil || paramString(params, key) == "" {
		return zero, false, nil
	}
	id, err := paramTypedID(params, key, parse)
	if err != nil {
		return zero, false, err
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
		// volume breaker) re-arms once the finding leaves the open states.
		return result, nil
	case recommend.ActionRevokeProductAccess:
		return result, executeRevokeProductAccess(r, params, result)
	case recommend.ActionGrantProduct:
		return result, executeGrantProduct(r, finding, params, result)
	case recommend.ActionCancelAndRefund:
		return result, executeCancelAndRefund(r, finding, params, notes, result)
	default:
		return result, paramErrorf("unknown recommendation action %q", action)
	}
}

// executeRevokeProductAccess revokes one product-access window as of a time.
func executeRevokeProductAccess(r *httprequest.Request, params map[string]any, result map[string]any) error {
	svc := r.State.ProductAccessService
	if svc == nil {
		return errors.New("product access service unavailable")
	}
	ctx := r.Request.Context()
	accessID, err := paramUUID(params, "access_id")
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
	access, err := r.State.EntitlementService.GetAccessByID(ctx, accessID)
	if err != nil {
		return paramErrorf("product access %s not found", accessID)
	}
	if access.RevokedAt != nil {
		result["revoke"] = "noop_already_revoked"
		return nil
	}
	if _, err := svc.RevokeProductAccessAsOf(ctx, accessID, models.AccessRevokeAdmin, asOf); err != nil {
		return fmt.Errorf("revoke product access %s: %w", accessID, err)
	}
	result["revoke"] = "revoked"
	result["access_id"] = accessID.String()
	convergeAfterMutation(r, access.CustomerID)
	return nil
}

// executeGrantProduct grants the product free (makes freeloader access
// legitimate), once per finding.
func executeGrantProduct(r *httprequest.Request, finding reconcile.FindingRecord, params map[string]any, result map[string]any) error {
	svc := productAccessService(r)
	if svc == nil {
		return errors.New("product access service unavailable")
	}
	ctx := r.Request.Context()
	customerID, err := paramUUID(params, "customer_id")
	if err != nil {
		return err
	}
	productID, err := paramTypedID(params, "product_id", billing.ParseProductID)
	if err != nil {
		return err
	}
	admin, ok := r.Staff()
	if !ok {
		return errors.New("missing admin identity")
	}
	grant := productaccess.Grant{CustomerID: customerID, ProductID: productID.UUID(), Reason: grants.ReasonStaff, Actor: admin.Subject, IdempotencyKey: "finding:" + finding.ID.String()}
	if reason := paramString(params, "reason"); reason != "" {
		grant.Note = &reason
		result["reason"] = reason
	}
	window, created, err := svc.GrantProduct(ctx, grant)
	if err != nil {
		return fmt.Errorf("grant product: %w", err)
	}
	result["access_id"] = window.ID.String()
	result["grant_created"] = created
	convergeAfterMutation(r, window.CustomerID)
	return nil
}

// executeCancelAndRefund cancels first (idempotent), then refunds; a refund
// failure after the cancel is a partial failure that carries the completed
// cancel. Either id may be absent (a one-off duplicate has no subscription),
// not both.
func executeCancelAndRefund(r *httprequest.Request, finding reconcile.FindingRecord, params map[string]any, notes string, result map[string]any) error {
	typedSubID, hasCancel, err := paramOptionalTypedID(params, "subscription_id", billing.ParseSubscriptionID)
	if err != nil {
		return err
	}
	typedPaymentID, hasRefund, err := paramOptionalTypedID(params, "refund_payment_id", billing.ParsePaymentID)
	if err != nil {
		return err
	}
	subID, paymentID := typedSubID.UUID(), typedPaymentID.UUID()
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

	var commands []destructive.Command
	if hasCancel {
		sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subID)
		if err != nil {
			return err
		}
		if sub.Status != models.StatusCanceled {
			if sub.CollectionPolicy != models.CollectionPolicyEngine && sub.Rail == models.RailSolana {
				return subscriptions.ErrCustomerActionRequired
			}
			command, err := subscriptions.CancellationCommand(r.Request.Context(), sub, reason, true, false)
			if err != nil {
				return err
			}
			commands = append(commands, command)
		}
	}
	if hasRefund {
		request, err := findingRefundRequest(params, reason)
		if err != nil {
			return err
		}
		command, err := adminRefundCommand(r.Request.Context(), r, paymentID, request, findingRefundKey(finding.ID, paymentID))
		if err != nil {
			return err
		}
		if _, found, err := preparedRefundReplay(r.Request.Context(), r, paymentID, findingRefundKey(finding.ID, paymentID)); err != nil {
			return err
		} else if !found {
			commands = append(commands, command)
		}
	}
	if len(commands) > 0 {
		if r.State.DestructiveAdmissions == nil {
			return errors.New("destructive finding admission is not wired")
		}
		actions, err := r.State.DestructiveAdmissions.AdmitBatch(r.Request.Context(), commands)
		if err != nil {
			return err
		}
		original := r.Request
		r.Request = original.WithContext(destructive.BindPreparedActions(original.Context(), actions))
		defer func() { r.Request = original }()
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

// cancelSubscriptionForFinding cancels one local subscription through the
// lifecycle chokepoint and schedules its provider cancel in the same
// transaction; the breaker gates the intent's execution, never its enqueue.
func cancelSubscriptionForFinding(r *httprequest.Request, subID uuid.UUID, reason string, result map[string]any) error {
	if r.State.AdminSubscriptionService == nil || r.State.SubscriptionService == nil {
		return errors.New("subscription services unavailable")
	}
	ctx := r.Request.Context()
	sub, err := r.State.SubscriptionService.GetByID(ctx, subID)
	if err != nil {
		return fmt.Errorf("load subscription %s: %w", subID, err)
	}
	if sub.Status == models.StatusCanceled {
		result["cancel"] = "noop_already_canceled"
		return nil
	}
	if err := r.State.AdminSubscriptionService.CancelSubscription(ctx, subID, reason, true, false); err != nil {
		return err
	}
	result["cancel"] = "canceled"
	result["subscription_id"] = billing.SubscriptionID(subID).String()
	if sub.RailSubscriptionID != "" {
		result["provider_cancel"] = "queued"
	}
	return nil
}

// refundPaymentForFinding refunds through the admin refund producer, as POST
// /v1/admin/payments/{id}/refunds does. The idempotency key derives from the
// finding, so a re-approve retries the same refund.
func refundPaymentForFinding(r *httprequest.Request, finding reconcile.FindingRecord, paymentID uuid.UUID, params map[string]any, reason string, result map[string]any) error {
	if r.State.PaymentService == nil {
		return errors.New("payment service unavailable")
	}
	ctx := r.Request.Context()
	if _, err := r.State.PaymentService.GetByID(ctx, paymentID); err != nil {
		return paramErrorf("refund payment %s not found", paymentID)
	}
	request, err := findingRefundRequest(params, reason)
	if err != nil {
		return err
	}
	idempotencyKey := findingRefundKey(finding.ID, paymentID)
	refund, status, err := executeAdminRefund(ctx, r, paymentID, request, idempotencyKey)
	if err != nil {
		return fmt.Errorf("refund payment %s: %w", paymentID, err)
	}
	result["refund_payment_id"] = billing.PaymentID(paymentID).String()
	if refund != nil {
		result["refund_amount"] = strconv.FormatInt(-refund.Amount, 10)
	}
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

func findingRefundKey(finding, payment uuid.UUID) string {
	return "finding:" + finding.String() + ":refund:" + payment.String()
}

func findingRefundRequest(params map[string]any, reason string) (RefundRequest, error) {
	request := RefundRequest{Full: true, Reason: reason}
	if raw, ok := params["amount"]; ok && raw != nil {
		amount, err := paramAmountMicros(raw)
		if err != nil {
			return request, err
		}
		request = RefundRequest{Amount: amount, Reason: reason}
	}
	if revoke, ok := params["revoke_access"].(bool); ok {
		request.RevokeAccess = revoke
	}
	return request, nil
}
