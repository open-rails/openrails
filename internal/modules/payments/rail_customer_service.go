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
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
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
	// Only rails with a person-level remote customer object (Stripe cus_*)
	// get a psp_customers row. NMI vaults are per-card, CCBill keys on
	// subscription_id, Solana on the wallet; their handles live on
	// payment_methods and subscriptions.
	if !railHasRemoteCustomer(rail) {
		return nil
	}
	// The mapping is per PSP: an unresolved PSP would overwrite a sibling
	// account's mapping.
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("upsert rail customer %s/%s: %w", rail, customerID, err)
	}
	// The customer row the mapping references.
	customerRowID, err := db.EnsureCustomerID(ctx, s.DB.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	return s.DB.Gen(ctx).UpsertPSPCustomer(ctx, gen.UpsertPSPCustomerParams{
		MerchantID:        tid.UUID(),
		CustomerID:        customerRowID,
		PspID:             pspID,
		RemoteCustomerRef: customerID,
		At:                now,
	})
}

// railHasRemoteCustomer reports whether a rail has a card-independent remote
// customer object worth a psp_customers row.
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
	tsid, err := db.ResolveCustomerID(userID)
	if err != nil {
		return "", err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	// Rail-scoped, not PSP-scoped: this is the portal/collection read, whose
	// callers legitimately hold no PSP. The query orders by recency so two
	// accounts on one rail resolve deterministically instead of arbitrarily.
	return s.DB.Gen(ctx).GetPSPCustomerRefForRail(ctx, gen.GetPSPCustomerRefForRailParams{
		MerchantID: tid.UUID(), CustomerID: tsid, Rail: rail,
	})
}

// GetAccountIDForPSP returns the exact PSP-owned remote customer id for a
// payable merchant subject. Webhook repair paths use it when an event omits the
// former customer id but the local payment-method evidence still identifies
// the subject.
func (s *RailCustomerService) GetAccountIDForPSP(ctx context.Context, customerID uuid.UUID, rail string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("rail customer service not initialized")
	}
	rail = strings.TrimSpace(rail)
	if customerID == uuid.Nil || rail == "" {
		return "", fmt.Errorf("invalid rail customer args")
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve rail customer %s for PSP: %w", rail, err)
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return s.DB.Gen(ctx).GetPSPCustomerRef(ctx, gen.GetPSPCustomerRefParams{
		MerchantID: merchantID.UUID(), CustomerID: customerID, PspID: pspID,
	})
}

// GetUserIDByCustomerID resolves the platform user from a rail customer id,
// for webhooks that carry the customer id but no user_id metadata. A remote
// customer id is unique only within the account that minted it, so the lookup
// is scoped to the PSP the webhook plane pinned on ctx.
func (s *RailCustomerService) GetUserIDByCustomerID(ctx context.Context, rail, customerID string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("rail customer service not initialized")
	}
	rail = strings.TrimSpace(rail)
	customerID = strings.TrimSpace(customerID)
	if rail == "" || customerID == "" {
		return "", fmt.Errorf("invalid rail customer args")
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return "", fmt.Errorf("reverse rail customer %s/%s: %w", rail, customerID, err)
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return s.DB.Gen(ctx).GetPSPCustomerByRef(ctx, gen.GetPSPCustomerByRefParams{
		MerchantID: tid.UUID(), PspID: pspID, RemoteCustomerRef: customerID,
	})
}
