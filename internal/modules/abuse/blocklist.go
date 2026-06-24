// Package abuse holds payment-fraud / abuse controls (issue #300).
//
// This slice provides ONLY the blocklist primitive: a merchant-scoped list of
// known-bad payment identifiers (card fingerprints, rail customer ids,
// emails, IPs) that checkout/admission can later consult to DENY a payment.
//
// INTEGRATION HOOK (out of this slice): the deny wiring is intentionally NOT
// built here. A checkout/admission gate should, before authorizing a charge,
// call BlocklistService.IsBlocked for each available identifier (card
// fingerprint, rail customer id, payer email, request IP) and refuse the
// charge if any returns true. Velocity caps and a new-account default tier are
// also out of scope for this slice.
package abuse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Valid blocklist identifier kinds. Mirrors the CHECK constraint on
// openrails.payment_blocklist.kind (migration 067).
const (
	KindCardFingerprint = "card_fingerprint"
	KindRailCustomer    = "processor_customer"
	KindEmail           = "email"
	KindIP              = "ip"
)

// ErrInvalidKind is returned when a blocklist operation is given a kind that is
// not one of the four supported identifier kinds.
var ErrInvalidKind = errors.New("invalid blocklist kind")

func validKind(kind string) bool {
	switch kind {
	case KindCardFingerprint, KindRailCustomer, KindEmail, KindIP:
		return true
	default:
		return false
	}
}

// BlocklistService manages the merchant-scoped payment blocklist (issue #300).
// All operations are RLS-scoped to the request merchant
// (merchant.Require) via the db.DB merchant-connection helpers, exactly
// like the credits service.
type BlocklistService struct {
	db *db.DB
}

// NewBlocklistService constructs a BlocklistService over the shared DB handle.
func NewBlocklistService(database *db.DB) *BlocklistService {
	return &BlocklistService{db: database}
}

// Add records a block for (kind, value). When the payer subject is nil (or zero) the block
// is merchant-wide (applies to every merchant subject in the merchant); when set the block
// is scoped to that merchant subject. reason is an optional free-form note.
//
// Add is idempotent on (merchant, kind, value): re-adding the same identifier is a
// no-op (the existing row and its payer scoping are left untouched).
func (s *BlocklistService) Add(ctx context.Context, payer *identity.CustomerID, kind, value, reason string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("blocklist service not initialized")
	}
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	if !validKind(kind) {
		return ErrInvalidKind
	}
	if value == "" {
		return fmt.Errorf("value required")
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	var customerID *uuid.UUID
	if payer != nil && !payer.IsZero() {
		id := payer.UUID()
		customerID = &id
	}
	var reasonPtr *string
	if r := strings.TrimSpace(reason); r != "" {
		reasonPtr = &r
	}

	entry := &models.PaymentBlocklistEntry{
		ID:         uuidutil.NewV7(),
		MerchantID: tenantID,
		CustomerID: customerID,
		Kind:       kind,
		Value:      value,
		Reason:     reasonPtr,
		CreatedAt:  time.Now().UTC(),
	}

	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		// Owner-scoped blocks reference a payable merchant subject; materialize its
		// customers row so the payment_blocklist FK (migration 076) holds (#317).
		if customerID != nil {
			if _, err := repo.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, customerID.String()); err != nil {
				return err
			}
		}
		return s.db.Gen(ctx).InsertPaymentBlockIfAbsent(ctx, gen.InsertPaymentBlockIfAbsentParams{
			ID:         entry.ID,
			MerchantID: entry.MerchantID,
			CustomerID: entry.CustomerID,
			Kind:       entry.Kind,
			Value:      entry.Value,
			Reason:     entry.Reason,
			CreatedAt:  entry.CreatedAt,
		})
	})
}

// Remove deletes the block for (kind, value) in the request merchant, if present.
// Removing a non-existent entry is a no-op.
func (s *BlocklistService) Remove(ctx context.Context, kind, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("blocklist service not initialized")
	}
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	if !validKind(kind) {
		return ErrInvalidKind
	}
	if value == "" {
		return fmt.Errorf("value required")
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Gen(ctx).DeletePaymentBlock(ctx, gen.DeletePaymentBlockParams{
			MerchantID: tenantID, Kind: kind, Value: value,
		})
	})
}

// IsBlocked reports whether (kind, value) is blocked in the request merchant. It
// returns true if a matching TENANT-WIDE row (customer_id IS NULL) OR ANY
// subject-scoped row exists for that (kind, value).
func (s *BlocklistService) IsBlocked(ctx context.Context, kind, value string) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("blocklist service not initialized")
	}
	kind = strings.TrimSpace(kind)
	value = strings.TrimSpace(value)
	if !validKind(kind) {
		return false, ErrInvalidKind
	}
	if value == "" {
		return false, fmt.Errorf("value required")
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	tenantID := tid.UUID()
	var blocked bool
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		exists, err := s.db.Gen(ctx).PaymentBlockExists(ctx, gen.PaymentBlockExistsParams{
			MerchantID: tenantID, Kind: kind, Value: value,
		})
		if err != nil {
			return err
		}
		blocked = exists
		return nil
	})
	if err != nil {
		return false, err
	}
	return blocked, nil
}
