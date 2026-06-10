// Package abuse holds payment-fraud / abuse controls (issue #300).
//
// This slice provides ONLY the blocklist primitive: a tenant-scoped list of
// known-bad payment identifiers (card fingerprints, processor customer ids,
// emails, IPs) that checkout/admission can later consult to DENY a payment.
//
// INTEGRATION HOOK (out of this slice): the deny wiring is intentionally NOT
// built here. A checkout/admission gate should, before authorizing a charge,
// call BlocklistService.IsBlocked for each available identifier (card
// fingerprint, processor customer id, payer email, request IP) and refuse the
// charge if any returns true. Velocity caps and a new-account default tier are
// also out of scope for this slice.
package abuse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Valid blocklist identifier kinds. Mirrors the CHECK constraint on
// billing.payment_blocklist.kind (migration 067).
const (
	KindCardFingerprint   = "card_fingerprint"
	KindProcessorCustomer = "processor_customer"
	KindEmail             = "email"
	KindIP                = "ip"
)

// ErrInvalidKind is returned when a blocklist operation is given a kind that is
// not one of the four supported identifier kinds.
var ErrInvalidKind = errors.New("invalid blocklist kind")

func validKind(kind string) bool {
	switch kind {
	case KindCardFingerprint, KindProcessorCustomer, KindEmail, KindIP:
		return true
	default:
		return false
	}
}

// BlocklistService manages the tenant-scoped payment blocklist (issue #300).
// All operations are RLS-scoped to the request tenant
// (tenant.FromContextOrDefault) via the db.DB tenant-connection helpers, exactly
// like the credits service.
type BlocklistService struct {
	db *db.DB
}

// NewBlocklistService constructs a BlocklistService over the shared DB handle.
func NewBlocklistService(database *db.DB) *BlocklistService {
	return &BlocklistService{db: database}
}

// Add records a block for (kind, value). When the payer subject is nil (or zero) the block
// is tenant-wide (applies to every tenant subject in the tenant); when set the block
// is scoped to that tenant subject. reason is an optional free-form note.
//
// Add is idempotent on (tenant, kind, value): re-adding the same identifier is a
// no-op (the existing row and its payer scoping are left untouched).
func (s *BlocklistService) Add(ctx context.Context, payer *identity.TenantSubjectID, kind, value, reason string) error {
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

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	var payerTenantID *uuid.UUID
	if payer != nil && !payer.IsZero() {
		id := payer.UUID()
		payerTenantID = &id
	}
	var reasonPtr *string
	if r := strings.TrimSpace(reason); r != "" {
		reasonPtr = &r
	}

	entry := &models.PaymentBlocklistEntry{
		ID:              uuidutil.NewV7(),
		TenantID:        tenantID,
		TenantSubjectID: payerTenantID,
		Kind:            kind,
		Value:           value,
		Reason:          reasonPtr,
		CreatedAt:       time.Now().UTC(),
	}

	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		// Owner-scoped blocks reference a payable tenant subject; materialize its
		// tenant_subjects row so the payment_blocklist FK (migration 076) holds (#317).
		if payerTenantID != nil {
			if _, err := repo.EnsureTenantSubjectID(ctx, s.db.Qx(ctx), tenantID, payerTenantID.String()); err != nil {
				return err
			}
		}
		_, err := s.db.Q(ctx).NewInsert().
			Model(entry).
			On("CONFLICT (tenant_id, kind, value) DO NOTHING").
			Exec(ctx)
		return err
	})
}

// Remove deletes the block for (kind, value) in the request tenant, if present.
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

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Q(ctx).NewDelete().
			Model((*models.PaymentBlocklistEntry)(nil)).
			Where("tenant_id = ?", tenantID).
			Where("kind = ? AND value = ?", kind, value).
			Exec(ctx)
		return err
	})
}

// IsBlocked reports whether (kind, value) is blocked in the request tenant. It
// returns true if a matching TENANT-WIDE row (tenant_subject_id IS NULL) OR ANY
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

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	var blocked bool
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		count, err := s.db.Q(ctx).NewSelect().
			Model((*models.PaymentBlocklistEntry)(nil)).
			Where("tenant_id = ?", tenantID).
			Where("kind = ? AND value = ?", kind, value).
			Limit(1).
			Count(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		blocked = count > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return blocked, nil
}
