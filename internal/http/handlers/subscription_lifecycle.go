package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// A customer's cancel carries their reason: 4 to 500 characters.
const (
	minCancelReasonChars = 4
	maxCancelReasonChars = 500
)

// CustomerCancelSubscriptionParams is the customer's cancel, at period end.
// Signature completes a solana_sign_transactions next action: the signature
// of the cancel transaction the customer's wallet sent.
type CustomerCancelSubscriptionParams struct {
	Reason    string `json:"reason"`
	Signature string `json:"signature"`
}

// CancelSubscription cancels one of the customer's subscriptions at period
// end and answers the subscription. A rail that needs the customer's own step
// (Solana: the wallet signs the on-chain cancel) answers the unchanged
// subscription with next_action; the customer repeats the request with the
// signature.
func CancelSubscription(r *httprequest.Request) {
	var req CustomerCancelSubscriptionParams
	if !r.BindJSON(&req) {
		return
	}
	reason := strings.TrimSpace(req.Reason)
	switch n := utf8.RuneCountInString(reason); {
	case n < minCancelReasonChars:
		r.ErrorCode(billing.CodeInvalidParam, "Please tell us why you're canceling (at least 4 characters).")
		return
	case n > maxCancelReasonChars:
		r.ErrorCode(billing.CodeInvalidParam, "The cancellation reason must be 500 characters or fewer.")
		return
	}
	userID, sub, ok := ownSubscription(r)
	if !ok {
		return
	}
	ctx := db.WithPSPID(r.Request.Context(), sub.PspID)

	if sub.Rail == models.RailSolana {
		signature := strings.TrimSpace(req.Signature)
		if signature == "" {
			if r.State.SolanaPrepareCancelService == nil {
				r.ErrorCode(billing.CodeServiceUnavailable, "Solana recurring billing is not configured")
				return
			}
			prepared, err := r.State.SolanaPrepareCancelService.Prepare(ctx, sub.ID)
			if err != nil {
				r.APIError(solanaClientError(err))
				return
			}
			writeMySubscription(r, userID, sub.ID, &billing.NextAction{
				Type: "solana_sign_transactions", Transactions: []string{prepared.Transaction},
			})
			return
		}
		if r.State.SolanaRPCResolver == nil || r.State.SubscriptionLifecycleService == nil {
			r.ErrorCode(billing.CodeServiceUnavailable, "Solana recurring billing is not configured")
			return
		}
		svc := recurring.NewConfirmCancelService(r.State.SolanaRPCResolver.ChainReader(), r.State.SubscriptionLifecycleService)
		if err := svc.Confirm(ctx, sub.ID, signature, reason); err != nil {
			r.APIError(solanaClientError(err))
			return
		}
	} else {
		if req.Signature != "" {
			r.APIError(api.Coded(billing.CodeInvalidParam, "signature completes a wallet step; this subscription's rail has none").WithParam("signature"))
			return
		}
		if err := cancelForCustomer(ctx, r, userID, sub, reason); err != nil {
			writeRefusal(r, err, "failed to cancel subscription")
			return
		}
	}
	convergeAfter(ctx, r, sub, "subscription_cancel")
	writeMySubscription(r, userID, sub.ID, nil)
}

// cancelForCustomer records the customer's cancel at period end: the local
// cancel plus the provider's cancel (inline on Stripe, a durable intent
// committed with it on NMI and CCBill).
func cancelForCustomer(ctx context.Context, r *httprequest.Request, userID string, sub *models.Subscription, reason string) error {
	// A provider-billed schedule that cannot be deleted (destructive actions
	// disarmed) is refused with its operator finding.
	if _, err := subscriptions.RequireProviderCancelArmed(ctx, r.State.SubscriptionService.Database(), sub, false); err != nil {
		return err
	}
	if sub.CollectionPolicy != models.CollectionPolicyEngine && sub.Rail != models.RailStripe {
		if r.State.UserSubscriptionService == nil {
			return fmt.Errorf("user subscription service unavailable")
		}
		return r.State.UserSubscriptionService.CancelUserSubscription(ctx, userID, sub.ID, reason)
	}
	if r.State.SubscriptionLifecycleService == nil {
		return fmt.Errorf("subscription lifecycle service unavailable")
	}
	if sub.CollectionPolicy != models.CollectionPolicyEngine {
		stripeSvc := &subscriptions.StripeService{StripeClients: r.State.StripeClients, Config: r.State.Config, Rails: r.State.RailConfigs}
		if err := stripeSvc.CancelSubscription(ctx, sub.RailSubscriptionID); err != nil {
			return err
		}
	}
	return r.State.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		SubscriptionID:     &sub.ID,
		CancelType:         models.CancelTypeUser,
		CancelFeedback:     &reason,
		RefuseOwnedRenewal: true,
	})
}

// ResumeSubscription undoes the customer's own scheduled cancel on a
// reversible rail before the paid period ends, and answers the subscription.
func ResumeSubscription(r *httprequest.Request) {
	userID, sub, ok := ownSubscription(r)
	if !ok {
		return
	}
	if !resume(r, sub) {
		return
	}
	writeMySubscription(r, userID, sub.ID, nil)
}

// AdminResumeSubscription is the merchant's resume of a subscription.
func AdminResumeSubscription(r *httprequest.Request) {
	id, ok := subscriptionIDParam(r)
	if !ok {
		return
	}
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), id)
	if err != nil {
		writeSubscriptionLoadError(r, err)
		return
	}
	if !resume(r, sub) {
		return
	}
	writeMerchantSubscription(r, sub.ID)
}

// AdminCancelSubscriptionRequest is the merchant's cancel body.
type AdminCancelSubscriptionRequest = billing.CancelSubscriptionParams

// AdminCancelSubscription is the merchant's cancel; it answers the
// subscription.
func AdminCancelSubscription(r *httprequest.Request) {
	id, ok := subscriptionIDParam(r)
	if !ok {
		return
	}
	var req AdminCancelSubscriptionRequest
	if !r.BindJSON(&req) {
		return
	}
	if err := r.State.AdminSubscriptionService.CancelSubscription(r.Request.Context(), id, strings.TrimSpace(req.Reason), req.RevokeAccess, req.AccountDeletion); err != nil {
		writeRefusal(r, err, "failed to cancel subscription")
		return
	}
	writeMerchantSubscription(r, id)
}

// resume restores a resumable subscription, writing the refusal when it is
// not resumable or the resume fails.
func resume(r *httprequest.Request, sub *models.Subscription) bool {
	now := r.Clock.Now().UTC()
	if !subscriptions.Resumable(sub, now) {
		switch {
		case sub.Status != models.StatusCanceled:
			r.ErrorCode(billing.CodeInvalidParam, "subscription is not canceled")
		case subscriptions.CancelModeFor(sub, now) != subscriptions.CancelModeReversible:
			r.ErrorCode(billing.CodeInvalidParam, "resume unsupported for rail")
		default:
			r.ErrorCode(billing.CodeInvalidParam, "subscription can no longer be resumed")
		}
		return false
	}
	ctx := db.WithPSPID(r.Request.Context(), sub.PspID)
	if r.State.SubscriptionLifecycleService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "subscriptions are not configured")
		return false
	}
	var err error
	switch {
	case sub.CollectionPolicy == models.CollectionPolicyEngine || sub.Rail == models.RailStripe:
		if sub.CollectionPolicy != models.CollectionPolicyEngine {
			stripeSvc := &subscriptions.StripeService{StripeClients: r.State.StripeClients, Config: r.State.Config, Rails: r.State.RailConfigs}
			if err = stripeSvc.ResumeSubscription(ctx, sub.RailSubscriptionID); err != nil {
				break
			}
		}
		_, err = r.State.SubscriptionLifecycleService.ResumeMembership(ctx, &subscriptions.ResumeMembershipParams{SubscriptionID: sub.ID})
	case rails.IsNMI(sub.Rail):
		err = resumeNMI(ctx, r, sub)
	default:
		r.ErrorCode(billing.CodeInvalidParam, "resume unsupported for rail")
		return false
	}
	if err != nil {
		writeRefusal(r, err, "failed to resume subscription")
		return false
	}
	return true
}

// resumeNMI undoes an NMI cancel inside its undo window. The NMI schedule was
// never deleted (the delete is deferred), so nothing is sent to NMI: the
// pending delete intent is superseded, the subscription and its paid access
// are restored, and the deferred-delete schedule is cleared. The status flip
// to active is the guard that counts: the intent executor re-reads it and
// supersedes the delete on its own if the supersede here missed.
func resumeNMI(ctx context.Context, r *httprequest.Request, sub *models.Subscription) error {
	if n, err := intents.NewStore(r.State.DB).SupersedeBySubject(ctx, intents.TypeNMIDeleteSubscription, sub.ID,
		"cancellation undone (resume) for customer "+sub.CustomerID.String()); err != nil {
		log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).
			Warn("failed to supersede the scheduled NMI delete; the executor's relevance check remains the guard")
	} else if n > 0 {
		log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "superseded": n}).Info("superseded the deferred NMI delete on resume")
	}
	reactivated, err := r.State.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
		Rail:                sub.Rail,
		RailSubscriptionID:  sub.RailSubscriptionID,
		CurrentPeriodEndsAt: sub.CurrentPeriodEndsAt,
		// A canceled subscription is terminal to the lifecycle guard; the
		// Resumable predicate checked above is the gate.
		AllowTerminalReactivation: true,
	})
	if err != nil {
		return fmt.Errorf("reactivate NMI subscription: %w", err)
	}
	if reactivated != nil && reactivated.DeletionScheduledAt != nil {
		reactivated.DeletionScheduledAt = nil
		if err := r.State.SubscriptionService.Update(ctx, reactivated); err != nil {
			return fmt.Errorf("clear deferred delete schedule: %w", err)
		}
	}
	return nil
}

// convergeAfter reconciles the customer right after a lifecycle change, so
// the drift it implies is repaired now rather than at the next sweep.
// Best-effort: the sweep is the backstop.
func convergeAfter(ctx context.Context, r *httprequest.Request, sub *models.Subscription, operation string) {
	if r.State.DB == nil {
		return
	}
	if _, err := converge.AfterMutation(ctx, r.State.DB, billing.MerchantID(sub.MerchantID), sub.CustomerID, r.Clock); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"operation": operation, "merchant_id": sub.MerchantID, "customer_id": sub.CustomerID,
		}).Warn("inline converge after a subscription change failed; the sweep will reconcile")
	}
}

// subscriptionIDParam reads the {id} path parameter.
func subscriptionIDParam(r *httprequest.Request) (uuid.UUID, bool) {
	id, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid subscription ID").WithParam("id"))
		return uuid.Nil, false
	}
	return id.UUID(), true
}

// ownSubscription loads the {id} subscription when it is the signed-in
// customer's; another customer's subscription is not found.
func ownSubscription(r *httprequest.Request) (string, *models.Subscription, bool) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "User authentication required")
		return "", nil, false
	}
	id, ok := subscriptionIDParam(r)
	if !ok {
		return "", nil, false
	}
	sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), id)
	if err != nil {
		writeSubscriptionLoadError(r, err)
		return "", nil, false
	}
	if sub.CustomerID.String() != user.ID {
		writeRefusal(r, subscriptions.ErrSubscriptionNotFound, "subscription not found")
		return "", nil, false
	}
	return user.ID, sub, true
}

func writeSubscriptionLoadError(r *httprequest.Request, err error) {
	if db.IsNotFound(err) {
		err = subscriptions.ErrSubscriptionNotFound
	}
	writeRefusal(r, err, "failed to load subscription")
}

// writeMySubscription answers the customer's subscription as GET
// /v1/me/subscriptions/{id} serves it, with an optional next action.
func writeMySubscription(r *httprequest.Request, userID string, id uuid.UUID, next *billing.NextAction) {
	out, ok := mySubscription(r, userID, id)
	if !ok {
		return
	}
	out.NextAction = next
	r.SuccessJSON(out)
}

func mySubscription(r *httprequest.Request, userID string, id uuid.UUID) (billing.Subscription, bool) {
	subscription, err := r.State.UserSubscriptionService.GetUserSubscriptionByID(r.Request.Context(), userID, id)
	if err != nil {
		if errors.Is(err, subscriptions.ErrSubscriptionNotFound) {
			writeRefusal(r, err, "subscription not found")
		} else {
			r.InternalError("failed to retrieve subscription", err)
		}
		return billing.Subscription{}, false
	}
	out := subscription.View()
	payer, ok := selfAccountPayer(r)
	if !ok {
		return billing.Subscription{}, false
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return billing.Subscription{}, false
	}
	if out.Recovery, err = svc.SubscriptionRecovery(r.Request.Context(), payer, id); err != nil {
		r.InternalError("subscription recovery unavailable", err)
		return billing.Subscription{}, false
	}
	return out, true
}

// writeMerchantSubscription answers the subscription as GET
// /v1/admin/subscriptions/{id} serves it.
func writeMerchantSubscription(r *httprequest.Request, id uuid.UUID) {
	svc := r.State.AdminSubscriptionService
	if svc == nil {
		r.ErrorCode(billing.CodeInternalError, "admin subscription service unavailable")
		return
	}
	subscription, err := svc.GetSubscriptionByID(r.Request.Context(), id)
	if err != nil {
		writeRefusal(r, err, "failed to load subscription")
		return
	}
	r.SuccessJSON(subscriptionView(subscription, r.Clock.Now()))
}
