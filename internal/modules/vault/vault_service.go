package vault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

type VaultService struct {
	PaymentMethodService paymentMethodStore
	SubscriptionService  subscriptionReader
	NMIClients           map[string]*nmi.NMIClient
	MerchantSecrets      merchantSecretGetter
	ProviderSecrets      providerAccountSecretResolver
	Config               *config.Config
	Rails                config.RailSet
	DB                   *db.DB
	clock                clockwork.Clock
	newNMIClient         func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error)
}

type subscriptionReader interface {
	GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error)
}

type paymentMethodStore interface {
	Create(ctx context.Context, method *models.PaymentMethod) error
	Update(ctx context.Context, method *models.PaymentMethod) error
	Delete(ctx context.Context, id uuid.UUID) error
	GetByUserID(ctx context.Context, userID string) ([]*models.PaymentMethod, error)
}

type merchantSecretGetter interface {
	Get(ctx context.Context, merchantID merchant.ID, name string) (merchants.Secret, error)
}

type providerAccountSecretResolver interface {
	PrimaryProviderAccountSecretName(ctx context.Context, merchantID merchant.ID, providerType, environment, key string) (string, bool, error)
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *VaultService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *VaultService) SetMerchantSecretStore(store merchantSecretGetter) {
	if s == nil {
		return
	}
	s.MerchantSecrets = store
}

func (s *VaultService) SetProviderAccountSecretResolver(resolver providerAccountSecretResolver) {
	if s == nil {
		return
	}
	s.ProviderSecrets = resolver
}

func (s *VaultService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *VaultService) Clock() clockwork.Clock {
	return s.clock
}

type CreateVaultRequest struct {
	PaymentToken string
	Provider     string
	NameOnCard   string
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
	NameOnCard   *string
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
	LastFour     *string
	CardType     *string
	ExpiryDate   *string
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

func NewVaultService(pm *PaymentMethodService, sub subscriptionReader, nmiClients map[string]*nmi.NMIClient, dbx *db.DB, cfg *config.Config, rails config.RailSet, clocks ...clockwork.Clock) *VaultService {
	return &VaultService{
		PaymentMethodService: pm,
		SubscriptionService:  sub,
		NMIClients:           nmiClients,
		Config:               cfg,
		Rails:                rails,
		DB:                   dbx,
		clock:                timeutil.FirstClock(clocks...),
	}
}

// CreateVault creates a NMI customer vault and stores a local PaymentMethod
func (s *VaultService) CreateVault(ctx context.Context, userID string, req *CreateVaultRequest) (*models.PaymentMethod, error) {
	rail := strings.TrimSpace(strings.ToLower(req.Provider))
	if rail == "" {
		return nil, errors.New("provider is required")
	}

	client, err := s.resolveNMIClient(ctx, rail)
	if err != nil {
		return nil, fmt.Errorf("rail '%s' is not configured: %w", rail, err)
	}

	firstName, lastName := nmiNameParts(req.FirstName, req.LastName, req.NameOnCard)
	vaultData := nmi.CreateCustomerVaultData{
		PaymentToken: req.PaymentToken,
		FirstName:    firstName,
		LastName:     lastName,
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
		ID:                   uuidutil.NewV7(),
		CustomerID:           identity.CustomerIDFromString(userID).UUID(),
		Rail:                 models.Rail(rail),
		VaultID:              nmiResponse.CustomerVaultID,
		InitialTransactionID: "",
		CreatedAt:            s.now(),
		UpdatedAt:            s.now(),
		LastFour:             stringPtrOrNil(sanitizeLastFour(req.LastFour)),
		ExpiryDate:           stringPtrOrNil(sanitizeExpiryDate(req.ExpiryDate)),
		CardType:             stringPtrOrNil(sanitizeCardType(req.CardType)),
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

func (s *VaultService) resolveNMIClient(ctx context.Context, provider string) (*nmi.NMIClient, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return nil, errors.New("provider is required")
	}

	secretKey := ""
	if provider == "mobius" {
		secretKey = "production_key"
	}
	if secretKey != "" && s != nil && s.MerchantSecrets != nil && s.ProviderSecrets != nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		secretName, ok, err := s.ProviderSecrets.PrimaryProviderAccountSecretName(ctx, tid, config.RailTypeNMI, "live", secretKey)
		if err != nil {
			return nil, fmt.Errorf("resolve merchant NMI secret: %w", err)
		}
		if !ok {
			return nil, errors.New("missing scoped merchant NMI secret for provider account")
		}
		sec, err := s.MerchantSecrets.Get(ctx, tid, secretName)
		if err != nil {
			if !errors.Is(err, merchants.ErrSecretNotFound) {
				return nil, fmt.Errorf("load merchant NMI secret: %w", err)
			}
			return nil, errors.New("missing scoped merchant NMI secret for provider account")
		} else if value := strings.TrimSpace(sec.Value); value != "" {
			proc := cloneRailConfig(s.railConfig(provider))
			if proc == nil {
				proc = &config.RailConfig{Type: config.RailTypeNMI}
			}
			proc.Type = config.RailTypeNMI
			proc.SecurityKey = value
			return s.buildNMIClient(provider, proc.ToNMIProviderSettings(provider))
		}
		return nil, errors.New("missing scoped merchant NMI secret for provider account")
	}

	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil
		}
	}
	if proc := s.railConfig(provider); proc != nil && rails.IsNMIBacked(provider) {
		return s.buildNMIClient(provider, proc.ToNMIProviderSettings(provider))
	}
	return nil, errors.New("missing client")
}

func (s *VaultService) buildNMIClient(provider string, cfg *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	testMode := s != nil && s.Config != nil && s.Config.IsTestMode()
	if s != nil && s.newNMIClient != nil {
		return s.newNMIClient(provider, cfg, testMode)
	}
	return nmi.NewClient(provider, cfg, testMode)
}

func (s *VaultService) railConfig(name string) *config.RailConfig {
	if s == nil {
		return nil
	}
	return s.Rails.GetRail(name)
}

func cloneRailConfig(in *config.RailConfig) *config.RailConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.Tokens != nil {
		out.Tokens = make(map[string]config.TokenConfig, len(in.Tokens))
		for k, v := range in.Tokens {
			out.Tokens[k] = v
		}
	}
	return &out
}

func nmiNameParts(firstName, lastName, nameOnCard string) (string, string) {
	firstName = strings.TrimSpace(firstName)
	lastName = strings.TrimSpace(lastName)
	if firstName != "" || lastName != "" {
		return firstName, lastName
	}
	parts := strings.Fields(strings.TrimSpace(nameOnCard))
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.Join(parts[1:], " ")
}

func sanitizeLastFour(value string) string {
	digits := strings.Builder{}
	for _, r := range value {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	out := digits.String()
	if len(out) > 4 {
		out = out[len(out)-4:]
	}
	if len(out) != 4 {
		return ""
	}
	return out
}

func sanitizeCardType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 30 {
		return ""
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && r != ' ' && r != '-' {
			return ""
		}
	}
	return value
}

func sanitizeExpiryDate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 7 {
		return ""
	}
	for _, r := range value {
		if !unicode.IsDigit(r) && r != '/' && r != '-' {
			return ""
		}
	}
	return value
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// UpdateVault updates vault in NMI and updates local record timestamp
func (s *VaultService) UpdateVault(ctx context.Context, pm *models.PaymentMethod, req *UpdateVaultRequest) (*models.PaymentMethod, error) {
	// Use rail from the payment method.
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return nil, errors.New("payment method rail is required")
	}

	client, err := s.resolveNMIClient(ctx, rail)
	if err != nil {
		return nil, fmt.Errorf("rail '%s' is not configured: %w", rail, err)
	}

	upd := nmi.UpdateCustomerVaultData{CustomerVaultID: pm.VaultID}

	paymentTokenUpdated := false
	if req.PaymentToken != nil {
		trimmed := strings.TrimSpace(*req.PaymentToken)
		if trimmed != "" {
			upd.PaymentToken = trimmed
			paymentTokenUpdated = true
		}
	}

	if req.FirstName != nil {
		upd.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		upd.LastName = *req.LastName
	}
	if req.NameOnCard != nil && req.FirstName == nil && req.LastName == nil {
		upd.FirstName, upd.LastName = nmiNameParts("", "", *req.NameOnCard)
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
	if paymentTokenUpdated {
		applyUpdatedCardMetadata(pm, req)
	}
	pm.UpdatedAt = s.now()
	if err := s.PaymentMethodService.Update(ctx, pm); err != nil {
		log.WithError(err).WithField("vault_id", pm.VaultID).Error("Failed to update local vault record")
		return nil, fmt.Errorf("failed to update local vault record: %w", err)
	}
	log.WithField("vault_id", pm.VaultID).Info("Successfully updated payment vault")
	return pm, nil
}

func applyUpdatedCardMetadata(pm *models.PaymentMethod, req *UpdateVaultRequest) {
	pm.LastFour = sanitizedStringPtr(req.LastFour, sanitizeLastFour)
	pm.CardType = sanitizedStringPtr(req.CardType, sanitizeCardType)
	pm.ExpiryDate = sanitizedStringPtr(req.ExpiryDate, sanitizeExpiryDate)
}

func sanitizedStringPtr(value *string, sanitize func(string) string) *string {
	if value == nil {
		return nil
	}
	return stringPtrOrNil(sanitize(*value))
}

// DeleteVault deletes the vault remotely after ensuring no active subscriptions use it; deactivates locally
func (s *VaultService) DeleteVault(ctx context.Context, pm *models.PaymentMethod) error {
	subs, _, err := s.SubscriptionService.GetPaginatedByUserID(ctx, pm.CustomerID.String(), 1, 1000)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"vault_id": pm.VaultID, "user_id": pm.CustomerID.String()}).Error("Failed to check subscriptions for vault")
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

	// Use rail from the payment method
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return errors.New("payment method rail is required")
	}

	client, err := s.resolveNMIClient(ctx, rail)
	if err != nil {
		return fmt.Errorf("rail '%s' is not configured: %w", rail, err)
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
