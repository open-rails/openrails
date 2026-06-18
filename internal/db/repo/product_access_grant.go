package repo

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ProductAccessGrantRepo persists durable product ownership as `kind=ownership`
// rows in the #514 grant ledger — the single source of truth for all grant kinds
// (entitlement / credit / ownership). The legacy `product_access_grants` table is
// retired (#511): ownership has no separate projection, so the grant row IS the
// record. This repo maps the append-only grant/revoke events back to the
// status-column models.ProductAccessGrant shape the Service + callers expect, so
// nothing above this layer changed.
type ProductAccessGrantRepo struct {
	db *db.DB
}

func NewProductAccessGrantRepo(d *db.DB) *ProductAccessGrantRepo {
	return &ProductAccessGrantRepo{db: d}
}

// ledger builds a merchant-scoped grant ledger on this repo's (tx-scoped) conn.
func (r *ProductAccessGrantRepo) ledger(ctx context.Context, merchantID uuid.UUID) *grants.Ledger {
	return grants.New(r.db.Gen(ctx), merchantID)
}

// ownershipModel maps an ownership grant-event (+ its optional termination event)
// to the legacy ProductAccessGrant shape. status='revoked' iff a terminating
// event supersedes the grant; revoked_at / reason come from that event.
func ownershipModel(g gen.OpenrailsGrant, term *gen.OpenrailsGrant) models.ProductAccessGrant {
	var productID uuid.UUID
	if g.ProductID != nil {
		productID = *g.ProductID
	}
	m := models.ProductAccessGrant{
		ID:         g.ID,
		MerchantID: g.MerchantID,
		CustomerID: g.CustomerID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourceType(g.SourceType),
		SourceID:   g.SourceID,
		PaymentID:  g.PaymentID,
		Status:     models.ProductAccessStatusActive,
		StartsAt:   g.StartsAt,
		EndsAt:     g.EndsAt,
		CreatedAt:  g.CreatedAt,
		UpdatedAt:  g.CreatedAt, // grants are append-only; the row is never updated
	}
	if term != nil {
		m.Status = models.ProductAccessStatusRevoked
		ra := term.CreatedAt
		m.RevokedAt = &ra
		m.UpdatedAt = term.CreatedAt
		if term.Reason != nil {
			rr := models.ProductAccessRevokeReason(*term.Reason)
			m.RevokeReason = &rr
		}
	}
	return m
}

// ownershipGrants loads every ownership grant-event for a customer with its
// derived status (status='revoked' iff a termination event supersedes the grant),
// in one query that LEFT JOINs each grant to its termination event.
func (r *ProductAccessGrantRepo) ownershipGrants(ctx context.Context, merchantID, customerID uuid.UUID) ([]models.ProductAccessGrant, error) {
	rows, err := r.db.Gen(ctx).ListOwnershipGrantsWithStatus(ctx, gen.ListOwnershipGrantsWithStatusParams{
		MerchantID: merchantID, CustomerID: customerID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.ProductAccessGrant, 0, len(rows))
	for i := range rows {
		row := rows[i]
		var productID uuid.UUID
		if row.ProductID != nil {
			productID = *row.ProductID
		}
		m := models.ProductAccessGrant{
			ID:         row.ID,
			MerchantID: row.MerchantID,
			CustomerID: row.CustomerID,
			ProductID:  productID,
			SourceType: models.ProductAccessSourceType(row.SourceType),
			SourceID:   row.SourceID,
			PaymentID:  row.PaymentID,
			Status:     models.ProductAccessStatusActive,
			StartsAt:   row.StartsAt,
			EndsAt:     row.EndsAt,
			CreatedAt:  row.CreatedAt,
			UpdatedAt:  row.CreatedAt,
		}
		if row.RevokedAt != nil {
			m.Status = models.ProductAccessStatusRevoked
			m.RevokedAt = row.RevokedAt
			m.UpdatedAt = *row.RevokedAt
			if row.RevokeReason != nil {
				rr := models.ProductAccessRevokeReason(*row.RevokeReason)
				m.RevokeReason = &rr
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// Insert creates an ownership grant in the ledger. The grant row IS the ownership
// record (no projection); the caller's model is stamped with the new grant id.
func (r *ProductAccessGrantRepo) Insert(ctx context.Context, grant *models.ProductAccessGrant) error {
	if (grant.MerchantID == uuid.UUID{}) {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		grant.MerchantID = tid.UUID()
	}
	if err := ensureCustomerRow(ctx, r.db.Qx(ctx), grant.MerchantID, grant.CustomerID); err != nil {
		return err
	}
	productID := grant.ProductID
	g, err := r.ledger(ctx, grant.MerchantID).Grant(ctx, grants.GrantInput{
		Customer: grant.CustomerID,
		Product:  &productID,
		Kind:     grants.Ownership,
		Source:   grants.SourceType(grant.SourceType),
		SourceID: grant.SourceID,
		Payment:  grant.PaymentID,
		StartsAt: grant.StartsAt,
		EndsAt:   grant.EndsAt,
	})
	if err != nil {
		return err
	}
	grant.ID = g.ID
	grant.StartsAt = g.StartsAt
	grant.CreatedAt = g.CreatedAt
	grant.UpdatedAt = g.CreatedAt
	return nil
}

// GetBySource returns the ownership grant for a (user, product, source)
// idempotency key, or nil. Prefers a live grant; falls back to the most recent.
func (r *ProductAccessGrantRepo) GetBySource(ctx context.Context, userID string, productID uuid.UUID, sourceID string) (*models.ProductAccessGrant, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	all, err := r.ownershipGrants(ctx, tid.UUID(), tsid)
	if err != nil {
		return nil, err
	}
	var match *models.ProductAccessGrant
	for i := range all {
		g := all[i]
		if g.ProductID != productID || g.SourceID != sourceID {
			continue
		}
		if g.Status == models.ProductAccessStatusActive {
			gg := g
			return &gg, nil // a live grant for this source wins
		}
		gg := g
		match = &gg // remember the most recent (rows are created_at ASC)
	}
	return match, nil
}

// HasActiveAccess reports whether the user has a live, in-window ownership grant
// for the product at time t.
func (r *ProductAccessGrantRepo) HasActiveAccess(ctx context.Context, userID string, productID uuid.UUID, at time.Time) (bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return false, err
	}
	live, err := r.db.Gen(ctx).ListLiveGrantsByCustomer(ctx, gen.ListLiveGrantsByCustomerParams{
		MerchantID: tid.UUID(), CustomerID: tsid,
	})
	if err != nil {
		return false, err
	}
	for i := range live {
		if ownershipInWindow(live[i], productID, at) {
			return true, nil
		}
	}
	return false, nil
}

// ownershipInWindow reports whether a live grant grants product access at `at`.
func ownershipInWindow(g gen.OpenrailsGrant, productID uuid.UUID, at time.Time) bool {
	if g.Kind != string(grants.Ownership) || g.ProductID == nil || *g.ProductID != productID {
		return false
	}
	if at.Before(g.StartsAt) {
		return false
	}
	if g.EndsAt != nil && !at.Before(*g.EndsAt) {
		return false
	}
	return true
}

// ListActiveByUser returns the user's live, in-window ownership grants.
func (r *ProductAccessGrantRepo) ListActiveByUser(ctx context.Context, userID string, at time.Time) ([]models.ProductAccessGrant, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	live, err := r.db.Gen(ctx).ListLiveGrantsByCustomer(ctx, gen.ListLiveGrantsByCustomerParams{
		MerchantID: tid.UUID(), CustomerID: tsid,
	})
	if err != nil {
		return nil, err
	}
	out := make([]models.ProductAccessGrant, 0, len(live))
	for i := range live {
		g := live[i]
		if g.Kind != string(grants.Ownership) {
			continue
		}
		if g.ProductID == nil || at.Before(g.StartsAt) || (g.EndsAt != nil && !at.Before(*g.EndsAt)) {
			continue
		}
		out = append(out, ownershipModel(g, nil))
	}
	reverse(out) // most-recent first (live list is created_at ASC)
	return out, nil
}

// ListByUser returns ALL of the user's ownership grants (active + revoked),
// most recent first.
func (r *ProductAccessGrantRepo) ListByUser(ctx context.Context, userID string) ([]models.ProductAccessGrant, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	all, err := r.ownershipGrants(ctx, tid.UUID(), tsid)
	if err != nil {
		return nil, err
	}
	reverse(all)
	return all, nil
}

// GetByID returns a single ownership grant by id (merchant-scoped), or nil.
func (r *ProductAccessGrantRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.ProductAccessGrant, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	g, err := r.db.Gen(ctx).GetGrant(ctx, gen.GetGrantParams{MerchantID: tid.UUID(), ID: id})
	if err != nil {
		return nil, nil // not found
	}
	if g.Kind != string(grants.Ownership) || g.Event != "grant" {
		return nil, nil
	}
	// Resolve status from the customer's grant log (to populate revoked_at/reason).
	all, err := r.ownershipGrants(ctx, tid.UUID(), g.CustomerID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			m := all[i]
			return &m, nil
		}
	}
	m := ownershipModel(g, nil)
	return &m, nil
}

// RevokeByID revokes a single active ownership grant. Returns rows affected so
// callers stay idempotent (already-revoked / not-found / non-ownership => 0).
func (r *ProductAccessGrantRepo) RevokeByID(ctx context.Context, id uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	g, err := r.db.Gen(ctx).GetGrant(ctx, gen.GetGrantParams{MerchantID: tid.UUID(), ID: id})
	if err != nil || g.Kind != string(grants.Ownership) || g.Event != "grant" {
		return 0, nil
	}
	terminated, err := r.db.Gen(ctx).IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: tid.UUID(), GrantID: id})
	if err != nil {
		return 0, err
	}
	if terminated {
		return 0, nil // already revoked
	}
	if _, err := r.ledger(ctx, tid.UUID()).Revoke(ctx, id, string(reason)); err != nil {
		return 0, err
	}
	return 1, nil
}

// RevokeByPayment revokes all live ownership grants tied to a payment (refund /
// chargeback reversal). Returns the number revoked.
func (r *ProductAccessGrantRepo) RevokeByPayment(ctx context.Context, paymentID uuid.UUID, now time.Time, reason models.ProductAccessRevokeReason) (int64, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	ids, err := r.db.Gen(ctx).ListLiveOwnershipGrantIDsByPayment(ctx, gen.ListLiveOwnershipGrantIDsByPaymentParams{
		MerchantID: tid.UUID(), PaymentID: paymentID,
	})
	if err != nil {
		return 0, err
	}
	gl := r.ledger(ctx, tid.UUID())
	var n int64
	for _, id := range ids {
		if _, err := gl.Revoke(ctx, id, string(reason)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// reverse flips a slice in place (live/all lists come back created_at ASC; the
// API contract is most-recent first).
func reverse(s []models.ProductAccessGrant) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
