package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

type RailCustomerService struct {
	DB *db.DB
}

func NewRailCustomerService(database *db.DB) *RailCustomerService {
	return &RailCustomerService{DB: database}
}

func (s *RailCustomerService) Upsert(ctx context.Context, userID, rail, customerID string) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("rail customer service not initialized")
	}
	userID = strings.TrimSpace(userID)
	rail = strings.TrimSpace(rail)
	customerID = strings.TrimSpace(customerID)
	if userID == "" || rail == "" || customerID == "" {
		return fmt.Errorf("invalid rail customer args")
	}
	// #635/#682: only rails with a PERSON-level remote customer object get a
	// rail_customer_accounts row — Stripe (cus_*) only. NMI vault ids are per-card
	// instrument containers (deliberately minted one per card, #682), CCBill
	// keys on subscription_id, Solana on the wallet address; a row for any of
	// those would conflate an instrument/subscription/wallet with a person.
	// No-op for those rails: their durable handles live on payment_methods
	// (rail_customer_ref) and subscriptions (rail_subscription_id).
	if !railHasRemoteCustomer(rail) {
		return nil
	}
	now := time.Now().UTC()
	// Resolve the payable merchant subject for this (merchant, user) so the row carries
	// customer_id alongside the legacy user_id (#317).
	customerRowID, err := repo.EnsureCustomerID(ctx, s.DB.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	// id is generated explicitly: the upsert targets the merchant-scoped
	// (merchant_id, customer_id, rail) unique, not the pk.
	return s.DB.Gen(ctx).UpsertRailCustomerAccount(ctx, gen.UpsertRailCustomerAccountParams{
		ID:         uuidutil.NewV7(),
		MerchantID: tid.UUID(),
		CustomerID: customerRowID,
		Rail:       rail,
		AccountID:  customerID,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
}

// railHasRemoteCustomer reports whether a rail exposes a card-independent remote
// customer object worth materializing into rail_customer_accounts (#635). Registry-backed (#669).
func railHasRemoteCustomer(rail string) bool {
	return rails.HasRemoteCustomer(models.Rail(rail))
}

func (s *RailCustomerService) GetCustomerID(ctx context.Context, userID, rail string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("rail customer service not initialized")
	}
	userID = strings.TrimSpace(userID)
	rail = strings.TrimSpace(rail)
	if userID == "" || rail == "" {
		return "", fmt.Errorf("invalid rail customer args")
	}
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return "", err
	}
	return s.DB.Gen(ctx).GetRailCustomerAccountID(ctx, gen.GetRailCustomerAccountIDParams{
		CustomerID: tsid, Rail: rail,
	})
}

// GetUserIDByCustomerID reverses GetCustomerID: it resolves the platform user from a
// rail customer id. Used by webhook handlers (e.g. subscription invoices) whose
// payloads carry the customer id but not the user_id metadata.
func (s *RailCustomerService) GetUserIDByCustomerID(ctx context.Context, rail, customerID string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("rail customer service not initialized")
	}
	rail = strings.TrimSpace(rail)
	customerID = strings.TrimSpace(customerID)
	if rail == "" || customerID == "" {
		return "", fmt.Errorf("invalid rail customer args")
	}
	return s.DB.Gen(ctx).GetRailCustomerAccountSubject(ctx, gen.GetRailCustomerAccountSubjectParams{
		AccountID: customerID, Rail: rail,
	})
}
