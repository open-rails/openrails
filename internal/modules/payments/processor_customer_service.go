package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
)

type ProcessorCustomerService struct {
	DB *db.DB
}

func NewProcessorCustomerService(database *db.DB) *ProcessorCustomerService {
	return &ProcessorCustomerService{DB: database}
}

func (s *ProcessorCustomerService) Upsert(ctx context.Context, userID, processor, customerID string) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("processor customer service not initialized")
	}
	userID = strings.TrimSpace(userID)
	processor = strings.TrimSpace(processor)
	customerID = strings.TrimSpace(customerID)
	if userID == "" || processor == "" || customerID == "" {
		return fmt.Errorf("invalid processor customer args")
	}
	now := time.Now().UTC()
	_, err := s.DB.GetDB().NewInsert().Model(&models.ProcessorCustomer{
		UserID:     userID,
		Processor:  processor,
		CustomerID: customerID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}).On("CONFLICT (user_id, processor) DO UPDATE").
		Set("customer_id = EXCLUDED.customer_id").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (s *ProcessorCustomerService) GetCustomerID(ctx context.Context, userID, processor string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("processor customer service not initialized")
	}
	userID = strings.TrimSpace(userID)
	processor = strings.TrimSpace(processor)
	if userID == "" || processor == "" {
		return "", fmt.Errorf("invalid processor customer args")
	}
	var customerID string
	err := s.DB.GetDB().NewSelect().Model((*models.ProcessorCustomer)(nil)).
		Column("customer_id").
		Where("user_id = ? AND processor = ?", userID, processor).
		Scan(ctx, &customerID)
	if err != nil {
		return "", err
	}
	return customerID, nil
}
