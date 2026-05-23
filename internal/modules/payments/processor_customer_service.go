package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
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
	// id is a NOT NULL uuid pk; bun sends the struct's zero value rather than
	// falling back to the column's uuidv7() default, so generate it explicitly.
	// Without this every insert ships id=000…0 and the second distinct
	// (user_id, processor) row collides on the pk (the ON CONFLICT below only
	// covers (user_id, processor)).
	_, err := s.DB.GetDB().NewInsert().Model(&models.ProcessorCustomer{
		ID:         uuidutil.NewV7(),
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

// GetUserIDByCustomerID reverses GetCustomerID: it resolves the platform user from a
// processor customer id. Used by webhook handlers (e.g. subscription invoices) whose
// payloads carry the customer id but not the user_id metadata.
func (s *ProcessorCustomerService) GetUserIDByCustomerID(ctx context.Context, processor, customerID string) (string, error) {
	if s == nil || s.DB == nil {
		return "", fmt.Errorf("processor customer service not initialized")
	}
	processor = strings.TrimSpace(processor)
	customerID = strings.TrimSpace(customerID)
	if processor == "" || customerID == "" {
		return "", fmt.Errorf("invalid processor customer args")
	}
	var userID string
	err := s.DB.GetDB().NewSelect().Model((*models.ProcessorCustomer)(nil)).
		Column("user_id").
		Where("customer_id = ? AND processor = ?", customerID, processor).
		Order("updated_at DESC").
		Limit(1).
		Scan(ctx, &userID)
	if err != nil {
		return "", err
	}
	return userID, nil
}
