package entitlements

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
)

// FeatureService implements the Stripe-shaped entitlement-features layer (issue
// #245): feature definitions, product-feature attachments, and the
// active-entitlements read. It sits ON TOP of the temporal window ledger owned by
// EntitlementService — the window ledger (billing.entitlements) stays the source
// of truth for active access; this service names features and resolves a user's
// active windows into the Stripe-shaped active-entitlement view by joining the
// window's entitlement string to the feature's lookup_key.
type FeatureService struct {
	db    *db.DB
	repo  *repo.EntitlementFeatureRepo
	ents  *repo.EntitlementRepo
	clock clockwork.Clock
}

func NewFeatureService(database *db.DB, clocks ...clockwork.Clock) *FeatureService {
	return &FeatureService{
		db:    database,
		repo:  repo.NewEntitlementFeatureRepo(database),
		ents:  repo.NewEntitlementRepo(database),
		clock: firstClock(clocks...),
	}
}

func (s *FeatureService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// CreateFeatureParams is the input for defining a new entitlement feature.
type CreateFeatureParams struct {
	LookupKey string
	Name      string
	Active    *bool
	Metadata  map[string]any
}

// CreateFeature defines a new entitlement feature for the active tenant.
func (s *FeatureService) CreateFeature(ctx context.Context, p CreateFeatureParams) (*models.EntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	lookupKey := strings.TrimSpace(p.LookupKey)
	if lookupKey == "" {
		return nil, fmt.Errorf("lookup_key is required")
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	active := true
	if p.Active != nil {
		active = *p.Active
	}
	now := s.now()
	f := &models.EntitlementFeature{
		LookupKey: lookupKey,
		Name:      name,
		Active:    active,
		Metadata:  p.Metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateFeature(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// ListFeatures returns all features for the active tenant.
func (s *FeatureService) ListFeatures(ctx context.Context) ([]models.EntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	return s.repo.ListFeatures(ctx)
}

// GetFeature returns a feature by id.
func (s *FeatureService) GetFeature(ctx context.Context, id uuid.UUID) (*models.EntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	return s.repo.GetFeatureByID(ctx, id)
}

// UpdateFeatureParams is the input for updating a feature's mutable fields.
type UpdateFeatureParams struct {
	Name     *string
	Active   *bool
	Metadata map[string]any
}

// UpdateFeature applies name/active/metadata changes to an existing feature.
func (s *FeatureService) UpdateFeature(ctx context.Context, id uuid.UUID, p UpdateFeatureParams) (*models.EntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	f, err := s.repo.GetFeatureByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return nil, fmt.Errorf("name cannot be empty")
		}
		f.Name = name
	}
	if p.Active != nil {
		f.Active = *p.Active
	}
	if p.Metadata != nil {
		f.Metadata = p.Metadata
	}
	f.UpdatedAt = s.now()
	if err := s.repo.UpdateFeature(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// AttachFeatureParams is the input for attaching a feature to a product.
type AttachFeatureParams struct {
	ProductID            uuid.UUID
	EntitlementFeatureID uuid.UUID
	DurationDays         *int
	Metadata             map[string]any
}

// AttachFeatureToProduct creates a product_feature attachment (Stripe shape).
func (s *FeatureService) AttachFeatureToProduct(ctx context.Context, p AttachFeatureParams) (*models.ProductEntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	if (p.ProductID == uuid.UUID{}) {
		return nil, fmt.Errorf("product_id is required")
	}
	if (p.EntitlementFeatureID == uuid.UUID{}) {
		return nil, fmt.Errorf("entitlement_feature_id is required")
	}
	if p.DurationDays != nil && *p.DurationDays <= 0 {
		return nil, fmt.Errorf("duration_days must be > 0 (or omit for indefinite)")
	}
	now := s.now()
	pef := &models.ProductEntitlementFeature{
		ProductID:            p.ProductID,
		EntitlementFeatureID: p.EntitlementFeatureID,
		DurationDays:         p.DurationDays,
		Metadata:             p.Metadata,
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	if err := s.repo.AttachFeatureToProduct(ctx, pef); err != nil {
		return nil, err
	}
	return pef, nil
}

// DetachFeature removes a product_feature attachment by its id.
func (s *FeatureService) DetachFeature(ctx context.Context, productFeatureID uuid.UUID) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("feature service not initialized")
	}
	return s.repo.DetachFeature(ctx, productFeatureID)
}

// ListProductFeatures returns the feature attachments for a product.
func (s *FeatureService) ListProductFeatures(ctx context.Context, productID uuid.UUID) ([]models.ProductEntitlementFeature, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	return s.repo.ListProductFeatures(ctx, productID)
}

// ActiveEntitlement is the Stripe-shaped active-entitlement view: one entry per
// active entitlement window, carrying the feature lookup_key plus the OpenRails
// window/source fields that Stripe lacks. Feature is populated when a matching
// feature definition exists for the window's lookup_key.
type ActiveEntitlement struct {
	ID         uuid.UUID                    `json:"id"`
	UserID     string                       `json:"user_id"`
	LookupKey  string                       `json:"lookup_key"`
	Feature    *models.EntitlementFeature   `json:"feature,omitempty"`
	StartAt    time.Time                    `json:"start_at"`
	EndAt      *time.Time                   `json:"end_at,omitempty"`
	SourceType models.EntitlementSourceType `json:"source_type"`
	SourceID   *uuid.UUID                   `json:"source_id,omitempty"`
}

// GetActiveEntitlements resolves a user's active entitlement windows at `at` into
// the Stripe-shaped active-entitlement list. The window ledger remains the source
// of truth; each window's `entitlement` string is matched to a feature definition
// by lookup_key (features are looked up in one batched query). Windows whose
// lookup_key has no feature definition are still returned (Feature == nil) so host
// apps never silently lose access to a granted key.
func (s *FeatureService) GetActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]ActiveEntitlement, error) {
	if s == nil || s.ents == nil {
		return nil, fmt.Errorf("feature service not initialized")
	}
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("userID is required")
	}
	if at.IsZero() {
		at = s.now()
	}
	windows, err := s.ents.ListActiveRecords(ctx, userID, at)
	if err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		return []ActiveEntitlement{}, nil
	}

	// Batch-load the feature definitions for the distinct lookup_keys present in
	// the active windows, so the per-window resolution is a map lookup.
	keySet := make(map[string]struct{}, len(windows))
	for _, w := range windows {
		keySet[w.Entitlement] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	features, err := s.repo.ListFeaturesByLookupKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*models.EntitlementFeature, len(features))
	for i := range features {
		f := features[i]
		byKey[f.LookupKey] = &f
	}

	out := make([]ActiveEntitlement, 0, len(windows))
	for _, w := range windows {
		ae := ActiveEntitlement{
			ID:         w.ID,
			UserID:     w.UserID,
			LookupKey:  w.Entitlement,
			Feature:    byKey[w.Entitlement],
			StartAt:    w.StartAt,
			EndAt:      w.EndAt,
			SourceType: w.SourceType,
			SourceID:   w.SourceID,
		}
		out = append(out, ae)
	}
	return out, nil
}
