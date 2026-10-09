// Package entitlements owns product access windows and the keys customers
// derive from them. A customer holds a key while a live window of theirs
// covers a product that grants the key; nothing per customer stores keys.
package entitlements

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

type EntitlementService struct {
	db    *db.DB
	clock clockwork.Clock
}

func NewEntitlementService(db *db.DB, clocks ...clockwork.Clock) *EntitlementService {
	return &EntitlementService{db: db, clock: timeutil.FirstClock(clocks...)}
}

func (s *EntitlementService) withTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	return s.db.MerchantTx(ctx, fn)
}

// SetClock sets the clock for this service. Used for testing.
func (s *EntitlementService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *EntitlementService) Clock() clockwork.Clock {
	return s.clock
}

func (s *EntitlementService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *EntitlementService) ledger(q *gen.Queries, merchantID uuid.UUID) *grants.Ledger {
	l := grants.New(q, merchantID)
	l.SetClock(func() time.Time { return s.now().UTC() })
	return l
}

// ProductCoverage reports, across the customer's live windows of the products
// at at, whether one is indefinite and otherwise the latest end.
func (s *EntitlementService) ProductCoverage(ctx context.Context, userID string, products []uuid.UUID, at time.Time) (bool, *time.Time, error) {
	customer, err := db.ResolveCustomerID(userID)
	if err != nil {
		return false, nil, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, nil, err
	}
	row, err := s.db.Gen(ctx).ProductAccessCoverage(ctx, gen.ProductAccessCoverageParams{MerchantID: mid.UUID(), CustomerID: customer, ProductIds: products, At: at})
	if err != nil {
		return false, nil, err
	}
	if row.Indefinite || row.LatestEndAt.Year() <= 1 {
		return row.Indefinite, nil, nil
	}
	return false, &row.LatestEndAt, nil
}

// GetAccessByID reads one window.
func (s *EntitlementService) GetAccessByID(ctx context.Context, id uuid.UUID) (*models.ProductAccess, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetProductAccessByID(ctx, gen.GetProductAccessByIDParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return nil, err
	}
	return models.ProductAccessFromGen(row), nil
}

// ListLiveAccessBySubscriptions returns the live windows the subscriptions
// give at at, longest first per subscription.
func (s *EntitlementService) ListLiveAccessBySubscriptions(ctx context.Context, subscriptions []uuid.UUID, at time.Time) ([]*models.ProductAccess, error) {
	if len(subscriptions) == 0 {
		return nil, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(subscriptions))
	for i, id := range subscriptions {
		ids[i] = id.String()
	}
	rows, err := s.db.Gen(ctx).ListLiveAccessBySubscriptions(ctx, gen.ListLiveAccessBySubscriptionsParams{MerchantID: mid.UUID(), SourceIds: ids, AtTime: at})
	if err != nil {
		return nil, err
	}
	out := make([]*models.ProductAccess, 0, len(rows))
	for _, row := range rows {
		out = append(out, models.ProductAccessFromGen(row))
	}
	return out, nil
}

// FirstLiveAccess returns the customer's newest live window at at, or nil.
func (s *EntitlementService) FirstLiveAccess(ctx context.Context, customer uuid.UUID, at time.Time) (*models.ProductAccess, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListProductAccessPage(ctx, gen.ListProductAccessPageParams{MerchantID: mid.UUID(), CustomerID: customer, LiveOnly: true, AtTime: at, FetchLimit: 1})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	r := rows[0]
	return models.ProductAccessFromGen(gen.BillingProductAccess{
		ID: r.ID, MerchantID: r.MerchantID, CustomerID: r.CustomerID, ProductID: r.ProductID, GrantID: r.GrantID,
		SourceType: r.SourceType, SourceID: r.SourceID, PaymentID: r.PaymentID, StartsAt: r.StartsAt, EndsAt: r.EndsAt,
		RevokedAt: r.RevokedAt, RevokeReason: r.RevokeReason, DeletedAt: r.DeletedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}), nil
}

// AccessExistsBySource reports whether a source already gave the product. A
// purchase gives it once: a revoked or retracted window is final.
func (s *EntitlementService) AccessExistsBySource(ctx context.Context, sourceType models.AccessSourceType, sourceID string, product uuid.UUID) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return s.db.Gen(ctx).ProductAccessExistsBySource(ctx, gen.ProductAccessExistsBySourceParams{
		MerchantID: mid.UUID(), SourceType: string(sourceType), SourceID: sourceID, ProductID: product,
	})
}

// ListLiveProductsBySource lists the products a source gives now or later.
func (s *EntitlementService) ListLiveProductsBySource(ctx context.Context, sourceType models.AccessSourceType, sourceID string) ([]uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListLiveProductsBySource(ctx, gen.ListLiveProductsBySourceParams{MerchantID: mid.UUID(), SourceType: string(sourceType), SourceID: sourceID})
}

// PushAccessParams describes one access window of one product from one source.
type PushAccessParams struct {
	UserID     string
	CustomerID uuid.UUID
	ProductID  uuid.UUID

	// NotBefore delays the window. Duration windows start at the latest of
	// NotBefore, the end of the customer's live windows of the product and
	// now; fixed-end and indefinite windows start at NotBefore when supplied.
	NotBefore *time.Time

	// Exactly one of Indefinite, Duration and EndsAt.
	Indefinite bool
	Duration   *time.Duration
	EndsAt     *time.Time

	SourceType models.AccessSourceType
	// SourceID is the source's id: a payment or subscription id, or a free
	// grant's idempotency key.
	SourceID  string
	PaymentID *uuid.UUID

	// A free grant's attribution.
	Actor       string
	GrantReason grants.GrantReason
	Note        *string
}

// PushAccess records a source's window of a product. Duration purchases
// append to the customer's finite windows of the product; fixed-end and
// indefinite windows keep their own interval. Replay consults only the same
// source, so refunding another source cannot erase this one's access.
func (s *EntitlementService) PushAccess(ctx context.Context, p PushAccessParams) (*models.ProductAccess, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("entitlement service not initialized")
	}
	if p.UserID == "" || p.ProductID == uuid.Nil {
		return nil, fmt.Errorf("userID and product are required")
	}
	if p.SourceID == "" {
		return nil, fmt.Errorf("sourceID is required")
	}
	set := 0
	for _, v := range []bool{p.Indefinite, p.Duration != nil, p.EndsAt != nil} {
		if v {
			set++
		}
	}
	if set != 1 {
		return nil, fmt.Errorf("exactly one of Indefinite, Duration, or EndsAt must be set")
	}
	if p.Duration != nil && *p.Duration <= 0 {
		return nil, fmt.Errorf("duration must be > 0")
	}
	if p.EndsAt != nil && p.EndsAt.IsZero() {
		return nil, fmt.Errorf("endAt must be non-zero")
	}
	now := s.now().UTC()
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var created *models.ProductAccess
	err = s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		requested, err := db.ResolveCustomerID(p.UserID)
		if err != nil {
			return err
		}
		if p.CustomerID != uuid.Nil && p.CustomerID != requested {
			return errors.New("access customer contradicts user identity")
		}
		if p.CustomerID == uuid.Nil {
			if p.CustomerID, err = db.EnsureCustomerID(ctx, tx, uuid.Nil, p.UserID); err != nil {
				return err
			}
		}
		if err := LockAccessTimeline(ctx, tx, p.UserID, p.ProductID); err != nil {
			return err
		}
		q := gen.New(tx)
		l := s.ledger(q, merchantID.UUID())
		// A subscription's accepted payment interval is immutable, even when a
		// longer or indefinite grant already covers it. Exact interval replay
		// keeps every paid period without stacking or truncating overlap.
		if p.SourceType == models.AccessSourceSubscription && (p.EndsAt != nil || p.Indefinite) {
			start := now
			if p.NotBefore != nil {
				start = p.NotBefore.UTC()
			}
			if p.EndsAt != nil && !p.EndsAt.After(start) {
				return errors.New("endAt must be after access start")
			}
			subscription, err := uuid.Parse(p.SourceID)
			if err != nil {
				return fmt.Errorf("subscription source: %w", err)
			}
			if _, err := l.GrantSubscriptionWindow(ctx, p.CustomerID, subscription, p.ProductID, grants.Subscription, start, p.EndsAt); err != nil {
				return err
			}
			row, err := q.GetLatestProductAccessBySource(ctx, gen.GetLatestProductAccessBySourceParams{
				MerchantID: merchantID.UUID(), CustomerID: p.CustomerID, ProductID: p.ProductID,
				SourceType: string(p.SourceType), SourceID: p.SourceID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			created = models.ProductAccessFromGen(row)
			return nil
		}
		previous, err := q.GetLatestProductAccessBySource(ctx, gen.GetLatestProductAccessBySourceParams{
			MerchantID: merchantID.UUID(), CustomerID: p.CustomerID, ProductID: p.ProductID,
			SourceType: string(p.SourceType), SourceID: p.SourceID,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// A revoked grace allowance (a canceled engine renewal that was then
		// resumed) is granted again, never replayed as its revoked self.
		if err == nil && previous.RevokedAt != nil && p.SourceType == models.AccessSourceGrace {
			err = pgx.ErrNoRows
		}
		if err == nil && (previous.EndsAt == nil || p.Duration != nil ||
			(p.EndsAt != nil && !p.EndsAt.After(*previous.EndsAt))) {
			created = models.ProductAccessFromGen(previous)
			return nil
		}
		var tailEnd *time.Time
		if p.Duration != nil {
			if tailEnd, err = GetAccessTimelineTailEnd(ctx, tx, p.CustomerID, p.ProductID); err != nil {
				return err
			}
		}
		start := now
		if p.NotBefore != nil {
			nb := p.NotBefore.UTC()
			if p.EndsAt != nil || p.Indefinite || nb.After(start) {
				start = nb
			}
		}
		if tailEnd != nil && tailEnd.After(start) {
			start = *tailEnd
		}
		var endAt *time.Time
		switch {
		case p.Duration != nil:
			e := start.Add(*p.Duration)
			endAt = &e
		case p.EndsAt != nil:
			e := p.EndsAt.UTC()
			if !e.After(start) {
				return fmt.Errorf("endAt must be after access start")
			}
			endAt = &e
		}
		product := p.ProductID
		input := grants.GrantInput{
			Customer: p.CustomerID, Product: &product, Kind: grants.Access,
			Source: grants.SourceType(p.SourceType), SourceID: p.SourceID, Payment: p.PaymentID,
			StartsAt: start, EndsAt: endAt, Reason: p.Note, Actor: p.Actor, GrantReason: p.GrantReason,
		}
		var g gen.BillingGrant
		if p.SourceType == models.AccessSourceGrace {
			g, err = l.Grant(ctx, input)
		} else {
			g, _, err = l.GrantAccessOnce(ctx, input)
		}
		if err != nil {
			return err
		}
		if err := l.MaterializeGrant(ctx, g); err != nil {
			return err
		}
		window, err := q.GetProductAccessByGrant(ctx, gen.GetProductAccessByGrantParams{MerchantID: merchantID.UUID(), GrantID: g.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		created = models.ProductAccessFromGen(window)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// BoundSubscriptionAccess writes the PROVEN closure of a subscription's access
// (#691): live subscription windows end at endAt — advance-written on disk, so
// a dead system cannot extend a canceled sub — and windows starting at or
// after it are removed. Idempotent.
func (s *EntitlementService) BoundSubscriptionAccess(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	now := s.now().UTC()
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: mid.UUID(), ID: subscriptionID}); err == nil {
			if err := lockOwner(ctx, q, mid.UUID(), sub.CustomerID); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := q.SoftDeleteFutureProductAccessBySubscription(ctx, gen.SoftDeleteFutureProductAccessBySubscriptionParams{
			MerchantID: mid.UUID(), SourceID: subscriptionID.String(), EndsAt: endAt.UTC(), Now: now,
		}); err != nil {
			return err
		}
		return q.EndActiveProductAccessBySubscription(ctx, gen.EndActiveProductAccessBySubscriptionParams{
			MerchantID: mid.UUID(), SourceID: subscriptionID.String(), EndsAt: endAt.UTC(), Now: now,
		})
	})
}

func (s *EntitlementService) ExtendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	return s.extendActiveBySubscription(ctx, subscriptionID, endAt.UTC(), s.now().UTC())
}

// extendActiveBySubscription extends a subscription's live finite windows to
// endAt, never shortening one, and moves the customer's later windows of the
// product by the same extension.
func (s *EntitlementService) extendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, now time.Time) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		rows, err := q.ListExtendableSubscriptionAccess(ctx, gen.ListExtendableSubscriptionAccessParams{
			MerchantID: mid.UUID(), SourceID: subscriptionID.String(), EndsAt: endAt,
		})
		if err != nil {
			return err
		}
		for _, candidate := range rows {
			if err := LockAccessTimeline(ctx, tx, candidate.CustomerID.String(), candidate.ProductID); err != nil {
				return err
			}
			current, err := q.GetProductAccessByIDForUpdate(ctx, gen.GetProductAccessByIDForUpdateParams{MerchantID: mid.UUID(), ID: candidate.ID})
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if current.CustomerID != candidate.CustomerID || current.ProductID != candidate.ProductID || current.SourceID != subscriptionID.String() || current.SourceType != string(models.AccessSourceSubscription) {
				return errors.New("access window changed during extension")
			}
			if current.RevokedAt != nil || current.EndsAt == nil {
				continue
			}
			oldEnd := current.EndsAt.UTC()
			if !endAt.After(oldEnd) {
				continue
			}
			if !endAt.After(current.StartsAt) {
				return fmt.Errorf("cannot extend access before its start")
			}
			if err := ShiftAccessTimeline(ctx, tx, current.CustomerID, current.ProductID, oldEnd, endAt.Sub(oldEnd), now, []uuid.UUID{current.ID}); err != nil {
				return err
			}
			if err := q.UpdateProductAccessEndAtIfMatch(ctx, gen.UpdateProductAccessEndAtIfMatchParams{
				MerchantID: mid.UUID(), ID: current.ID, NewEndAt: endAt, Now: now, OldEndAt: oldEnd,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// EndActiveByPayment ends a payment's access: started windows are revoked at
// now and windows not yet started are removed; their grants record it.
func (s *EntitlementService) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, reason models.AccessRevokeReason) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	now := s.now().UTC()
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if payment, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: paymentID}); err == nil {
			if err := lockOwner(ctx, q, mid.UUID(), payment.CustomerID); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := q.RetractFutureProductAccessByPayment(ctx, gen.RetractFutureProductAccessByPaymentParams{
			MerchantID: mid.UUID(), PaymentID: paymentID, Now: now, EndsAt: now,
		}); err != nil {
			return err
		}
		return q.RevokeActiveProductAccessByPayment(ctx, gen.RevokeActiveProductAccessByPaymentParams{
			MerchantID: mid.UUID(), PaymentID: paymentID, EndsAt: now, Now: now, RevokeReason: string(reason),
		})
	})
}

func (s *EntitlementService) RevokeSourcesForSubscription(ctx context.Context, userID string, subscriptionID uuid.UUID, reason models.AccessRevokeReason, sourceTypes ...models.AccessSourceType) error {
	return s.RevokeSourcesForSubscriptionAsOf(ctx, userID, subscriptionID, s.now().UTC(), reason, sourceTypes...)
}

// RevokeSourcesForSubscriptionAsOf revokes a subscription's windows of the
// given sources as of asOf (the LIFE-plane grace_exhausted repair revokes as of
// when grace lapsed), and terminates their grants so the ledger agrees.
func (s *EntitlementService) RevokeSourcesForSubscriptionAsOf(ctx context.Context, userID string, subscriptionID uuid.UUID, asOf time.Time, reason models.AccessRevokeReason, sourceTypes ...models.AccessSourceType) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	at := asOf.UTC()
	for _, sourceType := range sourceTypes {
		products, err := s.ListLiveProductsBySource(ctx, sourceType, subscriptionID.String())
		if err != nil {
			return fmt.Errorf("list %s access: %w", sourceType, err)
		}
		st, sid := sourceType, subscriptionID.String()
		for _, product := range products {
			if err := s.RevokeAccess(ctx, RevokeAccessParams{
				UserID: userID, ProductID: product, SourceType: &st, SourceID: &sid, Reason: reason, AsOf: &at,
			}); err != nil {
				return fmt.Errorf("revoke %s access to %s: %w", sourceType, product, err)
			}
		}
	}
	if len(sourceTypes) == 0 {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customer, err := db.ResolveCustomerID(userID)
	if err != nil {
		return err
	}
	sources := make([]grants.SourceType, 0, len(sourceTypes))
	for _, st := range sourceTypes {
		sources = append(sources, grants.SourceType(st))
	}
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := lockOwner(ctx, q, mid.UUID(), customer); err != nil {
			return err
		}
		return s.ledger(q, mid.UUID()).RevokeBySourceAsOf(ctx, customer, grants.Access, sources, subscriptionID.String(), "subscription source revoked", at)
	})
}

type RevokeAccessParams struct {
	// Exactly one of AccessID or (UserID and ProductID).
	AccessID  *uuid.UUID
	UserID    string
	ProductID uuid.UUID

	// Optional source filters.
	SourceType *models.AccessSourceType
	SourceID   *string

	Reason models.AccessRevokeReason

	// AsOf is when the revocation takes effect; nil is now.
	AsOf *time.Time
}

// RevokeAccess ends access now: started windows are revoked, future ones
// removed. It never rewrites ends_at.
func (s *EntitlementService) RevokeAccess(ctx context.Context, p RevokeAccessParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	if p.AccessID == nil && (p.UserID == "" || p.ProductID == uuid.Nil) {
		return fmt.Errorf("accessID or (userID, product) is required")
	}
	if p.AccessID != nil && (p.UserID != "" || p.ProductID != uuid.Nil) {
		return fmt.Errorf("provide either accessID or (userID, product), not both")
	}
	now := s.now().UTC()
	if p.AsOf != nil {
		now = p.AsOf.UTC()
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if p.AccessID != nil {
			row, err := q.GetProductAccessByID(ctx, gen.GetProductAccessByIDParams{MerchantID: mid.UUID(), ID: *p.AccessID})
			if err != nil {
				return err
			}
			if err := LockAccessTimeline(ctx, tx, row.CustomerID.String(), row.ProductID); err != nil {
				return err
			}
			if row.RevokedAt != nil || row.DeletedAt != nil {
				return nil
			}
			if p.SourceType != nil && row.SourceType != string(*p.SourceType) || p.SourceID != nil && row.SourceID != *p.SourceID {
				return nil
			}
			if row.StartsAt.After(now) {
				return q.SoftDeleteProductAccessByID(ctx, gen.SoftDeleteProductAccessByIDParams{MerchantID: mid.UUID(), ID: row.ID, Now: now})
			}
			if row.EndsAt == nil || row.EndsAt.After(now) {
				_, err := q.RevokeProductAccessByID(ctx, gen.RevokeProductAccessByIDParams{MerchantID: mid.UUID(), ID: row.ID, Now: now, RevokeReason: string(p.Reason)})
				return err
			}
			return nil
		}
		if err := LockAccessTimeline(ctx, tx, p.UserID, p.ProductID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		customer, err := db.ResolveCustomerID(p.UserID)
		if err != nil {
			return err
		}
		var st *string
		if p.SourceType != nil {
			v := string(*p.SourceType)
			st = &v
		}
		if err := q.RevokeActiveProductTimeline(ctx, gen.RevokeActiveProductTimelineParams{
			MerchantID: mid.UUID(), CustomerID: customer, ProductID: p.ProductID, Now: now,
			RevokeReason: string(p.Reason), SourceType: st, SourceID: p.SourceID,
		}); err != nil {
			return err
		}
		return q.SoftDeleteFutureProductTimeline(ctx, gen.SoftDeleteFutureProductTimelineParams{
			MerchantID: mid.UUID(), CustomerID: customer, ProductID: p.ProductID, Now: now,
			SourceType: st, SourceID: p.SourceID,
		})
	})
}

// RevokeGrantedAccess revokes one window. A free grant is revoked in the
// grant ledger and its projection follows, so a future window of it does not
// come back; a purchase or subscription window is revoked on its own.
func (s *EntitlementService) RevokeGrantedAccess(ctx context.Context, access *models.ProductAccess, reason models.AccessRevokeReason) error {
	if access.SourceType != models.AccessSourceGrant {
		id := access.ID
		return s.RevokeAccess(ctx, RevokeAccessParams{AccessID: &id, Reason: reason})
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := LockAccessTimeline(ctx, tx, access.CustomerID.String(), access.ProductID); err != nil {
			return err
		}
		q := gen.New(tx)
		g, err := q.GetGrant(ctx, gen.GetGrantParams{MerchantID: mid.UUID(), ID: access.GrantID})
		if err != nil {
			return err
		}
		terminated, err := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: mid.UUID(), GrantID: g.ID})
		if err != nil {
			return err
		}
		l := s.ledger(q, mid.UUID())
		if !terminated {
			if _, err := l.Revoke(ctx, g.ID, string(reason)); err != nil {
				return err
			}
		}
		return l.MaterializeGrant(ctx, g)
	})
}

// lockOwner takes the customer's mutex before a writer touches several of
// their windows: every access writer locks the customer first, so writers of
// one customer never deadlock on its access version.
func lockOwner(ctx context.Context, q *gen.Queries, merchantID, customer uuid.UUID) error {
	_, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: merchantID, ID: customer})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
