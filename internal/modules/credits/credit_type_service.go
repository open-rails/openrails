package credits

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
)

var creditTypeNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

type CreditTypeService struct {
	repo *repo.CreditTypeRepo
}

func NewCreditTypeService(database *db.DB) *CreditTypeService {
	return &CreditTypeService{repo: repo.NewCreditTypeRepo(database)}
}

// Create creates a new credit type. The Name is immutable once created.
func (s *CreditTypeService) Create(ctx context.Context, ct *models.CreditType) error {
	if ct == nil {
		return fmt.Errorf("credit type is nil")
	}
	ct.Name = strings.TrimSpace(ct.Name)
	ct.DisplayName = strings.TrimSpace(ct.DisplayName)
	ct.Unit = strings.TrimSpace(ct.Unit)

	if ct.ID == uuid.Nil {
		ct.ID = uuid.New()
	}
	if ct.Name == "" || !creditTypeNameRE.MatchString(ct.Name) {
		return fmt.Errorf("invalid credit type name")
	}
	if ct.DisplayName == "" {
		return fmt.Errorf("display_name required")
	}
	if ct.Unit == "" {
		return fmt.Errorf("unit required")
	}
	if ct.DecimalPlaces < 0 || ct.DecimalPlaces > 18 {
		return fmt.Errorf("decimal_places must be between 0 and 18")
	}
	if ct.CreatedAt.IsZero() {
		ct.CreatedAt = time.Now().UTC()
	}

	// Default new credit types to active.
	ct.IsActive = true

	return s.repo.Create(ctx, ct)
}

func (s *CreditTypeService) GetByName(ctx context.Context, name string) (*models.CreditType, error) {
	return s.repo.GetByName(ctx, strings.TrimSpace(name))
}

func (s *CreditTypeService) List(ctx context.Context, activeOnly bool) ([]*models.CreditType, error) {
	return s.repo.List(ctx, activeOnly)
}

type CreditTypeUpdateParams struct {
	DisplayName *string
	IsActive    *bool
}

// Update updates mutable fields on a credit type.
//
// Note: Name/unit/decimal_places are treated as immutable because they affect display and
// downstream interpretation of stored balances/transactions.
func (s *CreditTypeService) Update(ctx context.Context, name string, params CreditTypeUpdateParams) (*models.CreditType, error) {
	ct, err := s.repo.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return nil, err
	}
	if params.DisplayName != nil {
		v := strings.TrimSpace(*params.DisplayName)
		if v == "" {
			return nil, fmt.Errorf("display_name cannot be empty")
		}
		ct.DisplayName = v
	}
	if params.IsActive != nil {
		ct.IsActive = *params.IsActive
	}
	if err := s.repo.Update(ctx, ct); err != nil {
		return nil, err
	}
	return ct, nil
}

func (s *CreditTypeService) Deactivate(ctx context.Context, name string) error {
	ct, err := s.repo.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	if !ct.IsActive {
		return nil
	}
	ct.IsActive = false
	if err := s.repo.Update(ctx, ct); err != nil {
		return err
	}
	return nil
}

func (s *CreditTypeService) Activate(ctx context.Context, name string) error {
	ct, err := s.repo.GetByName(ctx, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	if ct.IsActive {
		return nil
	}
	ct.IsActive = true
	if err := s.repo.Update(ctx, ct); err != nil {
		return err
	}
	return nil
}

var ErrCreditTypeInactive = errors.New("credit_type_inactive")
