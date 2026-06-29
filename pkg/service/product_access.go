package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// ProductAccessRecord is the service-layer equivalent of the merchant
// product-access lookup response.
type ProductAccessRecord struct {
	ID           uuid.UUID
	CustomerID   uuid.UUID
	ProductID    uuid.UUID
	ProductKey   string
	ProductName  string
	SourceType   string
	SourceID     string
	PaymentID    *uuid.UUID
	Status       string
	StartsAt     time.Time
	EndsAt       *time.Time
	RevokedAt    *time.Time
	RevokeReason *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ListProductAccess lists active product-access grants for a host subject.
func (s *Service) ListProductAccess(ctx context.Context, userID string) ([]ProductAccessRecord, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("user_id required")
	}
	if s == nil || s.rt == nil || s.rt.ProductAccessService == nil {
		return nil, fmt.Errorf("product access service unavailable")
	}
	grants, err := s.rt.ProductAccessService.ListAccessibleProducts(ctx, userID)
	if err != nil {
		return nil, err
	}
	return s.productAccessRecords(ctx, grants), nil
}

// HasProductAccess reports whether a host subject can access a product.
func (s *Service) HasProductAccess(ctx context.Context, userID string, productID uuid.UUID) (bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false, fmt.Errorf("user_id required")
	}
	if productID == uuid.Nil {
		return false, fmt.Errorf("product_id required")
	}
	if s == nil || s.rt == nil || s.rt.ProductAccessService == nil {
		return false, fmt.Errorf("product access service unavailable")
	}
	return s.rt.ProductAccessService.HasProductAccess(ctx, userID, productID)
}

func (s *Service) productAccessRecords(ctx context.Context, grants []models.ProductAccessGrant) []ProductAccessRecord {
	out := make([]ProductAccessRecord, 0, len(grants))
	cache := map[uuid.UUID]*models.Product{}
	for i := range grants {
		g := grants[i]
		rec := ProductAccessRecord{
			ID:         g.ID,
			CustomerID: g.CustomerID,
			ProductID:  g.ProductID,
			SourceType: string(g.SourceType),
			SourceID:   g.SourceID,
			PaymentID:  g.PaymentID,
			Status:     string(g.Status),
			StartsAt:   g.StartsAt,
			EndsAt:     g.EndsAt,
			RevokedAt:  g.RevokedAt,
			CreatedAt:  g.CreatedAt,
			UpdatedAt:  g.UpdatedAt,
		}
		if g.RevokeReason != nil {
			reason := string(*g.RevokeReason)
			rec.RevokeReason = &reason
		}
		if s != nil && s.rt != nil && s.rt.ProductService != nil {
			prod, ok := cache[g.ProductID]
			if !ok {
				if p, err := s.rt.ProductService.GetByID(ctx, g.ProductID); err == nil {
					prod = p
				}
				cache[g.ProductID] = prod
			}
			if prod != nil {
				rec.ProductKey = prod.Key
				rec.ProductName = prod.DisplayName
			}
		}
		out = append(out, rec)
	}
	return out
}
