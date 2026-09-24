package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrProviderCancelHeld refuses a cancel that needs the provider's billing
// schedule deleted while destructive provider actions are disarmed: the local
// row would say cancelled while the provider kept charging.
var ErrProviderCancelHeld = apperr.New(http.StatusConflict, openrails.CodeProviderCancelHeld,
	"cancellation needs the provider's billing schedule deleted and destructive provider actions are not armed for this merchant; an operator has been notified")

// ErrPaymentMethodSameVault refuses repointing an NMI subscription at another
// card of the vault it already bills: NMI charges the vault's primary card.
var ErrPaymentMethodSameVault = apperr.New(http.StatusConflict, openrails.CodePaymentMethodSameVault,
	"this card is stored in the same provider vault the subscription already bills; save it as a new payment method to use it")

// ProviderCancelHeldFindingType is the operator finding a held cancel raises,
// one per subscription.
const ProviderCancelHeldFindingType = "life.provider_cancel.held"

// NeedsProviderScheduleDelete reports whether cancelling sub must delete a
// provider-owned NMI billing schedule.
func NeedsProviderScheduleDelete(sub *models.Subscription) bool {
	return sub != nil && sub.CollectionPolicy != models.CollectionPolicyEngine && rails.IsNMI(sub.Rail) && sub.RailSubscriptionID != ""
}

// RequireProviderCancelArmed admits a cancel of sub. A cancel that needs a
// provider schedule delete while the merchant is disarmed raises the held
// finding and is refused, unless accountDeletion: a deleted account is
// cancelled locally now and its delete waits for the operator's arming
// (held=true).
func RequireProviderCancelArmed(ctx context.Context, d *db.DB, sub *models.Subscription, accountDeletion bool) (held bool, err error) {
	if !NeedsProviderScheduleDelete(sub) {
		return false, nil
	}
	verdict := destructive.New(d).CheckMerchant(ctx, sub.MerchantID)
	if verdict.Allowed {
		return false, nil
	}
	if err := raiseProviderCancelHeld(ctx, d, sub, verdict.Reason, accountDeletion); err != nil {
		return false, err
	}
	if accountDeletion {
		return true, nil
	}
	return false, ErrProviderCancelHeld
}

func raiseProviderCancelHeld(ctx context.Context, d *db.DB, sub *models.Subscription, reason string, accountDeletion bool) error {
	evidence, err := json.Marshal(map[string]any{
		"subscription_id":      openrails.SubscriptionID(sub.ID).String(),
		"customer_id":          sub.CustomerID.String(),
		"rail":                 string(sub.Rail),
		"rail_subscription_id": sub.RailSubscriptionID,
		"account_deletion":     accountDeletion,
		"gate":                 reason,
	})
	if err != nil {
		return err
	}
	action := "A member asked to cancel a provider-billed subscription while destructive provider actions are disarmed; the provider schedule still bills. Arm destructive actions for this merchant, then cancel again."
	if accountDeletion {
		action = "An account was deleted; its subscription is cancelled locally but the provider schedule still bills until destructive provider actions are armed for this merchant (the held delete then runs)."
	}
	ctx = merchant.WithID(ctx, merchant.ID(sub.MerchantID))
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := db.NewWithPgxTx(tx).Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: sub.MerchantID, FindingType: ProviderCancelHeldFindingType, SubjectKey: sub.ID.String(),
			Severity: "critical", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
		})
		if err != nil {
			return fmt.Errorf("raise %s finding: %w", ProviderCancelHeldFindingType, err)
		}
		return nil
	})
}

// resolveProviderCancelHeld closes the subscription's held-cancel finding once
// a cancel with its provider delete is accepted.
func resolveProviderCancelHeld(ctx context.Context, d *db.DB, sub *models.Subscription) error {
	q := d.Gen(ctx)
	row, err := q.GetReconciliationFindingByIdentity(ctx, gen.GetReconciliationFindingByIdentityParams{MerchantID: sub.MerchantID, FindingType: ProviderCancelHeldFindingType, SubjectKey: sub.ID.String()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = q.MarkReconciliationFindingVanished(ctx, gen.MarkReconciliationFindingVanishedParams{MerchantID: sub.MerchantID, ID: row.ID})
	return err
}
