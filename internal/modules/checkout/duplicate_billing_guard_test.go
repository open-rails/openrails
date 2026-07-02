package checkout

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

// fakeSubscriptionAccess is an in-memory checkoutSubscriptionAccess for the
// duplicate-billing guard tests (issue #269). It returns whatever the test
// preloads, mimicking the repo's "active/pending/past_due only" filtering by
// only storing subscriptions the test intends to be non-terminal.
type fakeSubscriptionAccess struct {
	byPrice     map[uuid.UUID]*models.Subscription
	byTierGroup map[string]*models.Subscription
	byProduct   map[uuid.UUID]*models.Subscription
	// #691: unknown-status subs (parked pending provider verification).
	unknownByProduct   map[uuid.UUID]*models.Subscription
	unknownByTierGroup map[string]*models.Subscription
}

func (f *fakeSubscriptionAccess) GetUnknownByUserIDAndProductID(_ context.Context, _ string, productID uuid.UUID) (*models.Subscription, error) {
	if sub, ok := f.unknownByProduct[productID]; ok {
		return sub, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSubscriptionAccess) GetUnknownByUserIDAndTierGroup(_ context.Context, _ string, tierGroup string) (*models.Subscription, error) {
	if sub, ok := f.unknownByTierGroup[tierGroup]; ok {
		return sub, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSubscriptionAccess) GetByUserIDAndPriceID(_ context.Context, _ string, priceID uuid.UUID) (*models.Subscription, error) {
	if sub, ok := f.byPrice[priceID]; ok {
		return sub, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSubscriptionAccess) GetActiveOrPendingByUserIDAndTierGroup(_ context.Context, _ string, tierGroup string) (*models.Subscription, error) {
	if sub, ok := f.byTierGroup[tierGroup]; ok {
		return sub, nil
	}
	return nil, sql.ErrNoRows
}

func (f *fakeSubscriptionAccess) GetActiveOrPendingByUserIDAndProductID(_ context.Context, _ string, productID uuid.UUID) (*models.Subscription, error) {
	if sub, ok := f.byProduct[productID]; ok {
		return sub, nil
	}
	return nil, sql.ErrNoRows
}

func tierGroupPtr(s string) *string { return &s }

// existingSubFor builds a non-terminal subscription whose Price.Product carries
// the given product, as the repo loads it for tier comparison.
func existingSubFor(status models.SubscriptionStatus, product *models.Product, priceID uuid.UUID) *models.Subscription {
	return &models.Subscription{
		ID:      uuid.New(),
		Status:  status,
		PriceID: priceID,
		Price:   &models.Price{ID: priceID, Product: product},
	}
}

func TestCheckSubscriptionConflict(t *testing.T) {
	ctx := context.Background()

	groupID := "pro-plan"
	// Existing product the user is already subscribed to: $20 tier (rank 1).
	existingProduct := &models.Product{ID: uuid.New(), DisplayName: "Pro $20", TierGroup: tierGroupPtr(groupID), TierRank: 1}
	existingPriceID := uuid.New()

	// Target product in the SAME tier group at a higher tier (rank 2 = upgrade).
	sameGroupHigher := &models.Product{ID: uuid.New(), DisplayName: "Pro $50", TierGroup: tierGroupPtr(groupID), TierRank: 2}
	sameGroupHigherPrice := &models.Price{ID: uuid.New(), ProductID: sameGroupHigher.ID, Product: sameGroupHigher}

	// Same exact product+price the user already holds.
	existingPrice := &models.Price{ID: existingPriceID, ProductID: existingProduct.ID, Product: existingProduct}

	// A product in a DIFFERENT tier group.
	otherGroup := &models.Product{ID: uuid.New(), DisplayName: "Other", TierGroup: tierGroupPtr("other-group"), TierRank: 1}
	otherGroupPrice := &models.Price{ID: uuid.New(), ProductID: otherGroup.ID, Product: otherGroup}

	t.Run("same tier-group at a different tier is blocked", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			byTierGroup: map[string]*models.Subscription{
				groupID: existingSubFor(models.StatusActive, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", sameGroupHigherPrice, sameGroupHigher)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Blocked {
			t.Fatalf("expected a second sub in the same tier-group to be blocked, got %+v", got)
		}
	})

	t.Run("same exact price is blocked (idempotent re-subscribe)", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			byPrice: map[uuid.UUID]*models.Subscription{
				existingPriceID: existingSubFor(models.StatusActive, existingProduct, existingPriceID),
			},
			byTierGroup: map[string]*models.Subscription{
				groupID: existingSubFor(models.StatusActive, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", existingPrice, existingProduct)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Blocked || !got.SamePrice {
			t.Fatalf("expected same-exact-price re-subscribe blocked+SamePrice, got %+v", got)
		}
	})

	t.Run("a different tier-group is allowed", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			// User holds a sub in "pro-plan" group only.
			byTierGroup: map[string]*models.Subscription{
				groupID: existingSubFor(models.StatusActive, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", otherGroupPrice, otherGroup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Blocked {
			t.Fatalf("expected a different tier-group to be allowed, got %+v", got)
		}
	})

	t.Run("no existing subscription is allowed", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", sameGroupHigherPrice, sameGroupHigher)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Blocked {
			t.Fatalf("expected first-time subscribe to be allowed, got %+v", got)
		}
	})

	t.Run("an unknown sub for the same product blocks with the verification code (#691)", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			unknownByProduct: map[uuid.UUID]*models.Subscription{
				existingProduct.ID: existingSubFor(models.StatusUnknown, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", existingPrice, existingProduct)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Blocked || got.Code != ConflictCodeMembershipPendingVerification {
			t.Fatalf("expected pending-verification block, got %+v", got)
		}
	})

	t.Run("an unknown sub in the tier-group blocks with the verification code (#691)", func(t *testing.T) {
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			unknownByTierGroup: map[string]*models.Subscription{
				groupID: existingSubFor(models.StatusUnknown, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", sameGroupHigherPrice, sameGroupHigher)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got.Blocked || got.Code != ConflictCodeMembershipPendingVerification {
			t.Fatalf("expected pending-verification block, got %+v", got)
		}
	})

	t.Run("a terminal (cancelled) existing sub does not block", func(t *testing.T) {
		// The repo tier-group query already excludes cancelled, so it returns
		// nothing; the exact-price lookup may still surface a cancelled row, which
		// IsNonTerminalSubscriptionStatus must treat as non-blocking.
		svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
			byPrice: map[uuid.UUID]*models.Subscription{
				existingPriceID: existingSubFor(models.StatusCancelled, existingProduct, existingPriceID),
			},
		}}
		got, err := svc.CheckSubscriptionConflict(ctx, "user-1", existingPrice, existingProduct)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Blocked {
			t.Fatalf("expected a cancelled sub to allow re-subscribe, got %+v", got)
		}
	})
}

func TestCheckSubscriptionConflict_TierRankDirectionIsRelative(t *testing.T) {
	tests := []struct {
		name         string
		existingRank int
		targetRank   int
		wantMessage  string
	}{
		{name: "renumber upgrade", existingRank: 10, targetRank: 20, wantMessage: "tier upgrades"},
		{name: "renumber downgrade", existingRank: 20, targetRank: 10, wantMessage: "tier downgrades"},
		{name: "negative prepend upgrade", existingRank: -1, targetRank: 0, wantMessage: "tier upgrades"},
		{name: "negative prepend downgrade", existingRank: 0, targetRank: -1, wantMessage: "tier downgrades"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupID := "plans"
			existingProduct := &models.Product{ID: uuid.New(), DisplayName: "Existing", TierGroup: tierGroupPtr(groupID), TierRank: tt.existingRank}
			targetProduct := &models.Product{ID: uuid.New(), DisplayName: "Target", TierGroup: tierGroupPtr(groupID), TierRank: tt.targetRank}
			targetPrice := &models.Price{ID: uuid.New(), ProductID: targetProduct.ID, Product: targetProduct}
			existingPriceID := uuid.New()

			svc := &CheckoutPurchaseService{SubscriptionService: &fakeSubscriptionAccess{
				byTierGroup: map[string]*models.Subscription{
					groupID: existingSubFor(models.StatusActive, existingProduct, existingPriceID),
				},
			}}
			got, err := svc.CheckSubscriptionConflict(context.Background(), "user-1", targetPrice, targetProduct)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.Blocked || !strings.Contains(got.Message, tt.wantMessage) {
				t.Fatalf("want blocked %q message, got %+v", tt.wantMessage, got)
			}
		})
	}
}
