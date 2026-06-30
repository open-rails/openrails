package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
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
	// #635: only rails with a real card-independent remote CUSTOMER object get a
	// rail_customers row — Stripe (cus_*) and NMI (customer_vault_id). CCBill
	// keys on subscription_id and Solana on the wallet address; neither is a
	// customer, so a row there would conflate a subscription/wallet with a customer.
	// No-op for those rails: their durable handle is the subscription's
	// rail_subscription_id, not a fabricated rail_customer.
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
	return s.DB.Gen(ctx).UpsertRailCustomer(ctx, gen.UpsertRailCustomerParams{
		ID:             uuidutil.NewV7(),
		MerchantID:     tid.UUID(),
		CustomerID:     customerRowID,
		Rail:           rail,
		RailCustomerID: customerID,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
}

// railHasRemoteCustomer reports whether a rail exposes a card-independent remote
// customer object worth materializing into rail_customers (#635). Stripe (cus_*)
// and NMI (customer_vault_id) do; CCBill, Solana, and the rest do not.
func railHasRemoteCustomer(rail string) bool {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "stripe", "nmi":
		return true
	default:
		return false
	}
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
	return s.DB.Gen(ctx).GetRailCustomerID(ctx, gen.GetRailCustomerIDParams{
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
	return s.DB.Gen(ctx).GetRailCustomerSubject(ctx, gen.GetRailCustomerSubjectParams{
		RailCustomerID: customerID, Rail: rail,
	})
}
