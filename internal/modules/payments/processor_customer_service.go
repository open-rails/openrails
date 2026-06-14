package payments

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
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
	// Resolve the payable tenant subject for this (tenant, user) so the row carries
	// tenant_subject_id alongside the legacy user_id (#317).
	tenantSubjectID, err := repo.EnsureMerchantSubjectID(ctx, s.DB.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	// id is generated explicitly: the upsert targets the tenant-scoped
	// (tenant_id, tenant_subject_id, processor) unique, not the pk.
	return s.DB.Gen(ctx).UpsertProcessorCustomer(ctx, gen.UpsertProcessorCustomerParams{
		ID:              uuidutil.NewV7(),
		MerchantID:        tid.UUID(),
		MerchantSubjectID: tenantSubjectID,
		Processor:       processor,
		CustomerID:      customerID,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
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
	tsid, err := repo.ResolveMerchantSubjectID(userID)
	if err != nil {
		return "", err
	}
	return s.DB.Gen(ctx).GetProcessorCustomerID(ctx, gen.GetProcessorCustomerIDParams{
		MerchantSubjectID: tsid, Processor: processor,
	})
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
	return s.DB.Gen(ctx).GetProcessorCustomerSubject(ctx, gen.GetProcessorCustomerSubjectParams{
		CustomerID: customerID, Processor: processor,
	})
}
