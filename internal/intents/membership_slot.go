package intents

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// MembershipSlot is the one membership a customer holds of a product or of
// its tier group, as a purchase asks for it. Callers hold the customer lock.
type MembershipSlot struct {
	MerchantID, CustomerID, ProductID uuid.UUID
	At                                time.Time
	// Changes is the membership a change replaces; it holds the slot the
	// change takes over.
	Changes *uuid.UUID
	// Attempt is the Solana subscribe asking, whose row reserves the slot.
	Attempt uuid.UUID
}

// Refuse refuses a new membership while the slot is held: by a live
// subscription or a canceled one its provider may still bill, by a canceled
// one paid through At, by a purchase awaiting its first payment, or by an
// unpaid order.
func (s MembershipSlot) Refuse(ctx context.Context, q *gen.Queries) error {
	held, err := q.GetConflictingInitialEnrollmentSubscription(ctx, gen.GetConflictingInitialEnrollmentSubscriptionParams{MerchantID: s.MerchantID, CustomerID: s.CustomerID, ProductID: s.ProductID})
	switch {
	case err == nil && (s.Changes == nil || held.ID != *s.Changes):
		if held.Status == string(models.StatusCanceled) {
			return apperr.Conflictf("the customer's canceled subscription for this product or tier group may still bill at its provider until its stop is confirmed; resume it, or retry once the stop completes")
		}
		return apperr.Conflictf("customer already has a subscription for this product or tier group")
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if s.Changes == nil {
		runway, err := q.GetPaidRunwaySubscription(ctx, gen.GetPaidRunwaySubscriptionParams{MerchantID: s.MerchantID, CustomerID: s.CustomerID, ProductID: s.ProductID, Now: s.At})
		if err == nil {
			sub, err := models.SubscriptionFromGen(runway)
			if err != nil {
				return err
			}
			return subscriptions.PaidRunwayRefusal(sub, s.At)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	reason, err := s.Pending(ctx, q)
	if err != nil {
		return err
	}
	if reason != "" {
		return apperr.Conflictf("%s", reason)
	}
	if _, err := q.GetConflictingOrderClaim(ctx, gen.GetConflictingOrderClaimParams{MerchantID: s.MerchantID, CustomerID: s.CustomerID, ProductID: s.ProductID}); err == nil {
		return apperr.Conflictf("an unpaid order of the customer buys this product or tier group; pay or cancel it")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// Pending names another purchase of the slot awaiting its first payment: a
// Solana subscribe whose first pull may still land, or an unresolved
// enrollment. It is "" when there is none.
func (s MembershipSlot) Pending(ctx context.Context, q *gen.Queries) (string, error) {
	_, err := q.GetConflictingSubscribeAttempt(ctx, gen.GetConflictingSubscribeAttemptParams{ProductID: s.ProductID, MerchantID: s.MerchantID, CustomerID: s.CustomerID, OpenAfter: s.At.Add(-solana.LateSettlementWindow), ExceptID: s.Attempt})
	if err == nil {
		return "a Solana subscription to this product or tier group awaits its first payment", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	_, err = q.GetConflictingInitialEnrollmentOperation(ctx, gen.GetConflictingInitialEnrollmentOperationParams{MerchantID: s.MerchantID, CustomerID: s.CustomerID, ProductID: s.ProductID})
	if err == nil {
		return "another enrollment of this product or tier group is unresolved", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	return "", nil
}
