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
	// #635/#682: only rails with a PERSON-level remote customer object get a
	// psp_customers row — Stripe (cus_*) only. NMI vault ids are per-card
	// instrument containers (deliberately minted one per card, #682), CCBill
	// keys on subscription_id, Solana on the wallet address; a row for any of
	// those would conflate an instrument/subscription/wallet with a person.
	// No-op for those rails: their durable handles live on payment_methods
	// (rail_customer_ref) and subscriptions (rail_subscription_id).
	if !railHasRemoteCustomer(rail) {
		return nil
	}
	// or#893: the mapping is per-PSP, so the caller must have resolved which of
	// the merchant's accounts on this rail owns the remote customer object. Every
	// live caller does (the webhook plane pins it on ctx; checkout stamps it from
	// the routed target) — an unresolved one would silently overwrite a sibling
	// account's mapping.
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("upsert rail customer %s/%s: %w", rail, customerID, err)
	}
	// Resolve the payable merchant subject for this (merchant, user) so the row carries
	// customer_id alongside the legacy user_id (#317).
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

// railHasRemoteCustomer reports whether a rail exposes a card-independent remote
// customer object worth materializing into psp_customers (#635). Registry-backed (#669).
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

// GetUserIDByCustomerID reverses GetCustomerID: it resolves the platform user from a
// rail customer id. Used by webhook handlers (e.g. subscription invoices) whose
// payloads carry the customer id but not the user_id metadata.
//
// PSP-scoped (or#893): a remote customer id is only unique WITHIN the gateway
// account that minted it, so the reverse lookup must be told which account's
// payload it is reading. The webhook plane pins that on ctx when it routes.
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
