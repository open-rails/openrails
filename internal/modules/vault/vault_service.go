package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	log "github.com/sirupsen/logrus"
)

type VaultService struct {
	PaymentMethodService *PaymentMethodService
	SubscriptionService  subscriptionReader
	NMIClients           map[string]*nmi.NMIClient
	DB                   *db.DB
	Clock                clockwork.Clock
}

type subscriptionReader interface {
	GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error)
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *VaultService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

type CreateVaultRequest struct {
	PaymentToken string
	Provider     string
	FirstName    string
	LastName     string
	Address1     string
	City         string
	State        string
	Zip          string
	Country      string
	Phone        string
	Email        string
	Company      string
	Address2     string
	LastFour     string
	CardType     string
	ExpiryDate   string
	Metadata     map[string]any
}

type UpdateVaultRequest struct {
	PaymentToken *string
	Provider     *string
	FirstName    *string
	LastName     *string
	Address1     *string
	City         *string
	State        *string
	Zip          *string
	Country      *string
	Phone        *string
	Email        *string
	Company      *string
	Address2     *string
}

// VaultError carries additional context for vault creation failures, including localization codes.
type VaultError struct {
	Err            error
	LocalizationID string
	Message        string
}

func (e *VaultError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "vault error"
}

func (e *VaultError) Unwrap() error {
	return e.Err
}

func NewVaultService(pm *PaymentMethodService, sub subscriptionReader, nmiClients map[string]*nmi.NMIClient, dbx *db.DB) *VaultService {
	return &VaultService{
		PaymentMethodService: pm,
		SubscriptionService:  sub,
		NMIClients:           nmiClients,
		DB:                   dbx,
	}
}

// CreateVault creates a NMI customer vault and stores a local PaymentMethod
func (s *VaultService) CreateVault(ctx context.Context, userID string, req *CreateVaultRequest) (*models.PaymentMethod, error) {
	// Currently only mobius uses NMI vaults
	processor := "mobius"

	client, ok := s.NMIClients[processor]
	if !ok {
		return nil, fmt.Errorf("processor '%s' is not configured", processor)
	}

	vaultData := nmi.CreateCustomerVaultData{
		PaymentToken: req.PaymentToken,
		FirstName:    req.FirstName,
		LastName:     req.LastName,
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Phone:        req.Phone,
		Email:        req.Email,
		Company:      req.Company,
		Address2:     req.Address2,
	}

	nmiResponse, err := client.CreateCustomerVault(vaultData)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID}).Error("Failed to create vault in NMI")
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			return nil, &VaultError{
				Err:            err,
				LocalizationID: nmiErr.LocalizationID,
				Message:        fmt.Sprintf("failed to create payment vault: %s", err.Error()),
			}
		}
		return nil, fmt.Errorf("failed to create payment vault: %w", err)
	}

	pm := &models.PaymentMethod{
		ID:                   uuid.New(),
		UserID:               userID,
		Processor:            models.ProcessorMobius,
		VaultID:              nmiResponse.CustomerVaultID,
		InitialTransactionID: "",
		CreatedAt:            s.now(),
		UpdatedAt:            s.now(),
		LastFour:             &req.LastFour,
		ExpiryDate:           &req.ExpiryDate,
		CardType:             &req.CardType,
		Metadata:             req.Metadata,
	}

	if err := s.PaymentMethodService.Create(ctx, pm); err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID, "vault_id": nmiResponse.CustomerVaultID}).Error("Failed to store vault locally")
		// Attempt remote cleanup
		_ = client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: nmiResponse.CustomerVaultID})
		return nil, fmt.Errorf("failed to store vault locally: %w", err)
	}

	log.WithFields(log.Fields{"user_id": userID, "vault_id": pm.VaultID}).Info("Successfully created payment vault")
	return pm, nil
}

// UpdateVault updates vault in NMI and updates local record timestamp
func (s *VaultService) UpdateVault(ctx context.Context, pm *models.PaymentMethod, req *UpdateVaultRequest) (*models.PaymentMethod, error) {
	// Use processor from the payment method (mobius for NMI-backed vaults)
	processor := strings.ToLower(string(pm.Processor))
	if processor == "" {
		processor = "mobius"
	}

	client, ok := s.NMIClients[processor]
	if !ok {
		return nil, fmt.Errorf("processor '%s' is not configured", processor)
	}

	upd := nmi.UpdateCustomerVaultData{CustomerVaultID: pm.VaultID}

	if req.PaymentToken != nil {
		trimmed := strings.TrimSpace(*req.PaymentToken)
		if trimmed != "" {
			upd.PaymentToken = trimmed
		}
	}

	if req.FirstName != nil {
		upd.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		upd.LastName = *req.LastName
	}
	if req.Address1 != nil {
		upd.Address1 = *req.Address1
	}
	if req.City != nil {
		upd.City = *req.City
	}
	if req.State != nil {
		upd.State = *req.State
	}
	if req.Zip != nil {
		upd.Zip = *req.Zip
	}
	if req.Country != nil {
		upd.Country = *req.Country
	}
	if req.Phone != nil {
		upd.Phone = *req.Phone
	}
	if req.Email != nil {
		upd.Email = *req.Email
	}
	if req.Company != nil {
		upd.Company = *req.Company
	}
	if req.Address2 != nil {
		upd.Address2 = *req.Address2
	}

	if err := client.UpdateCustomerVault(upd); err != nil {
		log.WithError(err).WithField("vault_id", pm.VaultID).Error("Failed to update vault in NMI")
		return nil, fmt.Errorf("failed to update payment vault: %w", err)
	}

	pm.FailureReason = nil
	pm.UpdatedAt = s.now()
	if err := s.PaymentMethodService.Update(ctx, pm); err != nil {
		log.WithError(err).WithField("vault_id", pm.VaultID).Error("Failed to update local vault record")
		return nil, fmt.Errorf("failed to update local vault record: %w", err)
	}
	log.WithField("vault_id", pm.VaultID).Info("Successfully updated payment vault")
	return pm, nil
}

// DeleteVault deletes the vault remotely after ensuring no active subscriptions use it; deactivates locally
func (s *VaultService) DeleteVault(ctx context.Context, pm *models.PaymentMethod) error {
	subs, _, err := s.SubscriptionService.GetPaginatedByUserID(ctx, pm.UserID, 1, 1000)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"vault_id": pm.VaultID, "user_id": pm.UserID}).Error("Failed to check subscriptions for vault")
		return fmt.Errorf("failed to check vault usage: %w", err)
	}

	activeCount := 0
	for _, sub := range subs {
		if sub.Status == models.StatusActive || sub.Status == models.StatusPastDue {
			if sub.PaymentMethodID != nil && *sub.PaymentMethodID == pm.ID {
				activeCount++
			}
		}
	}
	if activeCount > 0 {
		return fmt.Errorf("cannot delete vault: %d active subscription(s) are using this payment method", activeCount)
	}

	// Use processor from the payment method
	processor := strings.ToLower(string(pm.Processor))
	if processor == "" {
		processor = "mobius"
	}

	client, ok := s.NMIClients[processor]
	if !ok {
		return fmt.Errorf("processor '%s' is not configured", processor)
	}

	if err := client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: pm.VaultID}); err != nil {
		log.WithError(err).WithField("vault_id", pm.VaultID).Error("Failed to delete vault from NMI")
		return fmt.Errorf("failed to delete payment vault: %w", err)
	}

	if err := s.PaymentMethodService.Delete(ctx, pm.ID); err != nil {
		log.WithError(err).WithField("vault_id", pm.VaultID).Error("Failed to delete vault locally")
		return fmt.Errorf("failed to delete local vault record: %w", err)
	}

	log.WithField("vault_id", pm.VaultID).Info("Successfully deleted payment vault")
	return nil
}

// GetUserVaults lists all vaults for a user
func (s *VaultService) GetUserVaults(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	return s.PaymentMethodService.GetByUserID(ctx, userID)
}
