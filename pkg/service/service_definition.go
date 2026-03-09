package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/services"
)

// -------------------------------- Definition Surface (Host/Admin) --------------------------------

type CreditType struct {
	Name          string
	DisplayName   string
	Unit          string
	DecimalPlaces int
	IsActive      bool
	CreatedAt     time.Time
}

type CreateCreditTypeRequest struct {
	Name          string
	DisplayName   string
	Unit          string
	DecimalPlaces int
}

func (s *Service) CreateCreditType(ctx context.Context, req CreateCreditTypeRequest) (*CreditType, error) {
	creditTypes, err := s.requireCreditTypeService()
	if err != nil {
		return nil, err
	}
	req.Name = strings.TrimSpace(req.Name)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Unit = strings.TrimSpace(req.Unit)
	if req.Name == "" {
		return nil, fmt.Errorf("name required")
	}
	if req.DisplayName == "" {
		return nil, fmt.Errorf("display_name required")
	}
	if req.Unit == "" {
		return nil, fmt.Errorf("unit required")
	}
	ct := &models.CreditType{
		Name:          req.Name,
		DisplayName:   req.DisplayName,
		Unit:          req.Unit,
		DecimalPlaces: req.DecimalPlaces,
	}
	if err := creditTypes.Create(ctx, ct); err != nil {
		return nil, err
	}
	return &CreditType{
		Name:          ct.Name,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: ct.DecimalPlaces,
		IsActive:      ct.IsActive,
		CreatedAt:     ct.CreatedAt,
	}, nil
}

type UpdateCreditTypeRequest struct {
	DisplayName *string
	IsActive    *bool
}

func (s *Service) UpdateCreditType(ctx context.Context, name string, req UpdateCreditTypeRequest) (*CreditType, error) {
	creditTypes, err := s.requireCreditTypeService()
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	ct, err := creditTypes.Update(ctx, name, services.CreditTypeUpdateParams{
		DisplayName: req.DisplayName,
		IsActive:    req.IsActive,
	})
	if err != nil {
		return nil, err
	}
	return &CreditType{
		Name:          ct.Name,
		DisplayName:   ct.DisplayName,
		Unit:          ct.Unit,
		DecimalPlaces: ct.DecimalPlaces,
		IsActive:      ct.IsActive,
		CreatedAt:     ct.CreatedAt,
	}, nil
}

func (s *Service) DeactivateCreditType(ctx context.Context, name string) error {
	creditTypes, err := s.requireCreditTypeService()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	return creditTypes.Deactivate(ctx, name)
}

func (s *Service) ActivateCreditType(ctx context.Context, name string) error {
	creditTypes, err := s.requireCreditTypeService()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name required")
	}
	return creditTypes.Activate(ctx, name)
}

func (s *Service) ListCreditTypes(ctx context.Context, activeOnly bool) ([]CreditType, error) {
	creditTypes, err := s.requireCreditTypeService()
	if err != nil {
		return nil, err
	}
	items, err := creditTypes.List(ctx, activeOnly)
	if err != nil {
		return nil, err
	}
	out := make([]CreditType, 0, len(items))
	for _, ct := range items {
		out = append(out, CreditType{
			Name:          ct.Name,
			DisplayName:   ct.DisplayName,
			Unit:          ct.Unit,
			DecimalPlaces: ct.DecimalPlaces,
			IsActive:      ct.IsActive,
			CreatedAt:     ct.CreatedAt,
		})
	}
	return out, nil
}
