package paymentmethods

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
	MerchantSecrets      merchants.MerchantSecretReader
	ProviderSecrets      merchants.RailMerchantAccountSecretResolver
	ProviderScopes       merchants.RailMerchantAccountScopeResolver
	Config               *config.Config
	Rails                config.RailMerchantAccountSet
	DB                   *db.DB
	// DeleteIntents routes DeleteVault through the durable nmi_vault_delete
	// provider intent (#674 tail); wired at runtime assembly.
	DeleteIntents VaultDeleteExecutor
	clock         clockwork.Clock
	newNMIClient  func(provider string, cfg *config.NMIProviderSettings, testMode bool) (*nmi.NMIClient, error)
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

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *VaultService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *VaultService) SetMerchantSecretStore(store merchants.MerchantSecretReader) {
	if s == nil {
		return
	}
	s.MerchantSecrets = store
}

func (s *VaultService) SetRailMerchantAccountSecretResolver(resolver merchants.RailMerchantAccountSecretResolver) {
	if s == nil {
		return
	}
	s.ProviderSecrets = resolver
	if scopes, ok := resolver.(merchants.RailMerchantAccountScopeResolver); ok {
		s.ProviderScopes = scopes
	}
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

func NewVaultService(pm *PaymentMethodService, sub subscriptionReader, nmiClients map[string]*nmi.NMIClient, dbx *db.DB, cfg *config.Config, rails config.RailMerchantAccountSet, clocks ...clockwork.Clock) *VaultService {
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

	client, railMerchantAccountID, err := s.resolveNMIClient(ctx, rail)
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
		ID:         uuidutil.NewV7(),
		CustomerID: identity.CustomerIDFromString(userID).UUID(),
		Rail:       models.Rail(rail),
		// #682 minting policy (deliberate): ONE NMI vault customer PER CARD —
		// the vault id is an instrument-scoped handle, never a person. NMI has
		// no person-level identity in our model (see rails registry
		// HasRemoteCustomer=false); the person is the local customer_id UUID.
		// Both handles are recorded verbatim; RebillDriver (not ref-emptiness)
		// carries the rebill-driver mode, defaulting to 'provider'.
		RailCustomerRef:      nmiResponse.CustomerVaultID,
		RailMethodRef:        nmiResponse.BillingID,
		RebillDriver:         models.RebillDriverProvider,
		InitialTransactionID: "",
		CreatedAt:            s.now(),
		UpdatedAt:            s.now(),
		LastFour:             stringPtrOrNil(sanitizeLastFour(req.LastFour)),
		ExpiryDate:           stringPtrOrNil(sanitizeExpiryDate(req.ExpiryDate)),
		CardType:             stringPtrOrNil(sanitizeCardType(req.CardType)),
		Metadata:             req.Metadata,
	}
	if railMerchantAccountID != nil {
		pm.RailMerchantAccountID = railMerchantAccountID
	}

	if err := s.PaymentMethodService.Create(ctx, pm); err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID, "vault_id": nmiResponse.CustomerVaultID}).Error("Failed to store vault locally")
		// Best-effort direct remote cleanup — deliberately NOT intent-routed
		// (#674 tail): the vault was created milliseconds ago and is referenced
		// nowhere; losing this delete leaves only an inert orphan entry at NMI.
		_ = client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: nmiResponse.CustomerVaultID})
		return nil, fmt.Errorf("failed to store vault locally: %w", err)
	}

	log.WithFields(log.Fields{"user_id": userID, "vault_id": pm.RailCustomerRef}).Info("Successfully created payment vault")
	return pm, nil
}

func (s *VaultService) resolveNMIClient(ctx context.Context, provider string, railMerchantAccountID ...*uuid.UUID) (*nmi.NMIClient, *uuid.UUID, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return nil, nil, errors.New("rail is required")
	}

	if len(railMerchantAccountID) > 0 && railMerchantAccountID[0] != nil {
		if s == nil || s.DB == nil {
			return nil, nil, errors.New("provider account lookup unavailable")
		}
		row, err := s.DB.Gen(ctx).GetRailMerchantAccount(ctx, *railMerchantAccountID[0])
		if err != nil {
			return nil, nil, err
		}
		if !rails.SameRail(models.Rail(row.Rail), models.Rail(provider)) {
			return nil, nil, fmt.Errorf("provider account %s belongs to rail %s, not %s", row.ID, row.Rail, provider)
		}
		client, err := s.resolveNMIClientForScope(ctx, merchants.RailMerchantAccountScope{
			ID:          row.ID,
			Rail:        row.Rail,
			Environment: row.Environment,
			AccountID:   row.AccountID,
		})
		return client, &row.ID, err
	}

	var scopeResolver merchants.RailMerchantAccountScopeResolver
	if s != nil {
		scopeResolver = s.ProviderScopes
		if scopeResolver == nil {
			if resolver, ok := s.ProviderSecrets.(merchants.RailMerchantAccountScopeResolver); ok {
				scopeResolver = resolver
			}
		}
	}
	if rails.IsNMI(models.Rail(provider)) && s != nil && s.MerchantSecrets != nil && scopeResolver != nil {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return nil, nil, err
		}
		// Environment follows deployment posture (#681): test rows under test_mode.
		env := config.ExpectedProviderEnvironment(s.Config != nil && s.Config.IsTestMode())
		scope, ok, err := scopeResolver.ActiveRailMerchantAccountScope(ctx, tid, string(models.RailNMI), env)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve merchant NMI account: %w", err)
		}
		if !ok {
			return nil, nil, errors.New("missing scoped merchant NMI provider account")
		}
		client, err := s.resolveNMIClientForScope(ctx, scope)
		if err != nil {
			return nil, nil, err
		}
		return client, &scope.ID, nil
	}

	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil, nil
		}
	}
	if rails.IsNMI(models.Rail(provider)) {
		if proc := s.activeNMIConfig(); proc != nil {
			client, err := s.buildNMIClient(provider, proc.ToNMIProviderSettings())
			return client, nil, err
		}
	}
	return nil, nil, errors.New("missing client")
}

func (s *VaultService) resolveNMIClientForScope(ctx context.Context, scope merchants.RailMerchantAccountScope) (*nmi.NMIClient, error) {
	provider := strings.TrimSpace(scope.AccountID)
	if provider == "" {
		return nil, errors.New("provider account_id required")
	}
	if s != nil && s.NMIClients != nil {
		if client := s.NMIClients[provider]; client != nil {
			return client, nil
		}
	}
	if s == nil || s.MerchantSecrets == nil {
		return nil, errors.New("missing scoped merchant NMI secret for provider account")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	secretName, err := merchants.RailMerchantAccountSecretName(scope.Rail, scope.Environment, scope.AccountID, "security_key")
	if err != nil {
		return nil, err
	}
	sec, err := s.MerchantSecrets.Get(ctx, tid, secretName)
	if err != nil {
		if !errors.Is(err, merchants.ErrSecretNotFound) {
			return nil, fmt.Errorf("load merchant NMI secret: %w", err)
		}
		return nil, errors.New("missing scoped merchant NMI secret for provider account")
	}
	value := strings.TrimSpace(sec.Value)
	if value == "" {
		return nil, errors.New("missing scoped merchant NMI secret for provider account")
	}
	proc := cloneRailConfig(s.railConfig(scope.AccountID))
	if proc == nil {
		proc = cloneRailConfig(s.activeNMIConfig())
	}
	if proc == nil {
		proc = &config.RailMerchantAccountConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{}}
	}
	proc.Rail = models.RailNMI
	if proc.NMI == nil {
		proc.NMI = &config.NMIRailConfig{}
	}
	proc.NMI.SecurityKey = value
	return s.buildNMIClient(provider, proc.ToNMIProviderSettings())
}

func (s *VaultService) buildNMIClient(provider string, cfg *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	testMode := s != nil && s.Config != nil && s.Config.IsTestMode()
	if s != nil && s.newNMIClient != nil {
		return s.newNMIClient(provider, cfg, testMode)
	}
	return nmi.NewClient(provider, cfg, testMode)
}

func (s *VaultService) railConfig(name string) *config.RailMerchantAccountConfig {
	if s == nil {
		return nil
	}
	return s.Rails.GetRail(name)
}

// activeNMIConfig returns the configured active NMI provider account, if any.
func (s *VaultService) activeNMIConfig() *config.RailMerchantAccountConfig {
	if s == nil {
		return nil
	}
	_, proc, _ := s.Rails.ActiveRailByType(models.RailNMI)
	return proc
}

func cloneRailConfig(in *config.RailMerchantAccountConfig) *config.RailMerchantAccountConfig {
	if in == nil {
		return nil
	}
	out := *in
	if in.Solana != nil && in.Solana.Tokens != nil {
		out.Solana = &config.SolanaRailConfig{}
		*out.Solana = *in.Solana
		out.Solana.Tokens = make(map[string]config.TokenConfig, len(in.Solana.Tokens))
		for k, v := range in.Solana.Tokens {
			out.Solana.Tokens[k] = v
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

	client, _, err := s.resolveNMIClient(ctx, rail, pm.RailMerchantAccountID)
	if err != nil {
		return nil, fmt.Errorf("rail '%s' is not configured: %w", rail, err)
	}

	upd := nmi.UpdateCustomerVaultData{CustomerVaultID: pm.RailCustomerRef}

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
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to update vault in NMI")
		return nil, fmt.Errorf("failed to update payment vault: %w", err)
	}

	if paymentTokenUpdated {
		applyUpdatedCardMetadata(pm, req)
	}
	pm.UpdatedAt = s.now()
	if err := s.PaymentMethodService.Update(ctx, pm); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to update local vault record")
		return nil, fmt.Errorf("failed to update local vault record: %w", err)
	}
	log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully updated payment vault")
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

// ErrVaultDeleteProcessing: the durable vault-delete intent could not confirm
// the remote delete inline (transport-ambiguous outcome, parked provider). The
// intent ledger completes it out-of-band; a retried request maps onto the SAME
// intent. Never a lost delete.
var ErrVaultDeleteProcessing = errors.New("payment method deletion is processing; it will complete automatically")

// VaultDeleteOutcome mirrors the durable intent's post-execution state without
// importing the intents package (import cycle: intents → subscriptions →
// paymentmethods). Neither Done nor Terminal = still resolving out-of-band.
type VaultDeleteOutcome struct {
	// Done: the remote delete is confirmed and the local row is gone.
	Done bool
	// Terminal: the delete failed permanently; Reason says why.
	Terminal bool
	Reason   string
}

// VaultDeleteExecutor posts the durable nmi_vault_delete intent and executes
// it inline (#674 write-through). Implemented by intents.VaultDeleteThrough.
type VaultDeleteExecutor interface {
	ExecuteVaultDelete(ctx context.Context, pm *models.PaymentMethod) (VaultDeleteOutcome, error)
}

// DeleteVault deletes a stored payment method the DURABLE way (#674 tail):
// guards run inline, then the remote NMI delete goes through the write-ahead
// nmi_vault_delete intent — a crash or lost response after the user's request
// is accepted can never lose the delete (the verifier resolves "vault gone at
// provider ⇒ done" and finalizes the local removal).
func (s *VaultService) DeleteVault(ctx context.Context, pm *models.PaymentMethod) error {
	if _, _, err := s.deleteVaultGuards(ctx, pm); err != nil {
		return err
	}
	if s.DeleteIntents == nil {
		return errors.New("vault delete intent executor not wired")
	}
	out, err := s.DeleteIntents.ExecuteVaultDelete(ctx, pm)
	if err != nil {
		return fmt.Errorf("post vault delete intent: %w", err)
	}
	switch {
	case out.Done:
		log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully deleted payment vault")
		return nil
	case out.Terminal:
		return fmt.Errorf("failed to delete payment vault: %s", out.Reason)
	default:
		return ErrVaultDeleteProcessing
	}
}

// CleanupVaultBestEffort is the DIRECT delete for reactive decline-cleanup
// (checkout removing a vault it just created for a now-declined attempt).
// Deliberately NOT intent-routed (#674 tail): the vault is referenced nowhere,
// a lost delete leaves only an inert orphan entry at NMI (no billing state can
// act on it), and card-testing decline floods must not spam the intent ledger
// or burn the #679 destructive-volume budget. Durable user-initiated deletes
// go through DeleteVault.
func (s *VaultService) CleanupVaultBestEffort(ctx context.Context, pm *models.PaymentMethod) error {
	shared, client, err := s.deleteVaultGuards(ctx, pm)
	if err != nil {
		return err
	}
	return s.deleteVaultDirect(ctx, client, pm, shared)
}

// deleteVaultGuards runs the shared pre-delete checks: no active subscription
// may use the method, the rail must resolve to a configured client (fail fast
// instead of parking a user-facing request), and a shared vault without a
// billing id is refused outright.
func (s *VaultService) deleteVaultGuards(ctx context.Context, pm *models.PaymentMethod) (shared bool, client *nmi.NMIClient, err error) {
	subs, _, err := s.SubscriptionService.GetPaginatedByUserID(ctx, pm.CustomerID.String(), 1, 1000)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"vault_id": pm.RailCustomerRef, "user_id": pm.CustomerID.String()}).Error("Failed to check subscriptions for vault")
		return false, nil, fmt.Errorf("failed to check vault usage: %w", err)
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
		return false, nil, fmt.Errorf("cannot delete vault: %d active subscription(s) are using this payment method", activeCount)
	}

	// Use rail from the payment method
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return false, nil, errors.New("payment method rail is required")
	}

	// #682 shared-vault handling: deleting the remote vault customer destroys
	// EVERY billing entry in it. Our minting policy is one vault per card, so
	// sharing only happens when an importer materialized a multi-card vault as
	// several payment-method rows — in that case delete ONLY this row's billing
	// entry (the vault survives for the sibling cards; NMI refuses to empty a
	// vault, so the LAST row's delete is the whole-vault delete).
	if s.DB != nil && strings.TrimSpace(pm.RailCustomerRef) != "" {
		n, err := NewPaymentMethodRepo(s.DB).CountSharingCustomerRef(ctx, rail, pm.RailCustomerRef, pm.ID)
		if err != nil {
			return false, nil, fmt.Errorf("failed to check vault sharing: %w", err)
		}
		shared = n > 0
	}
	if shared && strings.TrimSpace(pm.RailMethodRef) == "" {
		// Cannot identify WHICH entry is this card — refuse rather than
		// destroy the siblings (an importer that shares vaults must record
		// per-row billing ids, see #682's shared-vault contract).
		return shared, nil, fmt.Errorf("cannot delete vault %s: shared by other stored payment methods and this row carries no billing id to scope the delete", pm.RailCustomerRef)
	}

	client, _, err = s.resolveNMIClient(ctx, rail, pm.RailMerchantAccountID)
	if err != nil {
		return shared, nil, fmt.Errorf("rail '%s' is not configured: %w", rail, err)
	}
	return shared, client, nil
}

// deleteVaultDirect performs the remote delete (billing-entry-scoped for
// shared vaults, whole-vault otherwise) followed by the local removal.
func (s *VaultService) deleteVaultDirect(ctx context.Context, client *nmi.NMIClient, pm *models.PaymentMethod, shared bool) error {
	if shared {
		if err := client.DeleteCustomerBillingEntry(pm.RailCustomerRef, pm.RailMethodRef); err != nil {
			log.WithError(err).WithFields(log.Fields{"vault_id": pm.RailCustomerRef, "billing_id": pm.RailMethodRef}).Error("Failed to delete vault billing entry from NMI")
			return fmt.Errorf("failed to delete payment vault entry: %w", err)
		}
		return s.PaymentMethodService.Delete(ctx, pm.ID)
	}

	if err := client.DeleteCustomerVault(nmi.DeleteCustomerVaultData{CustomerVaultID: pm.RailCustomerRef}); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to delete vault from NMI")
		return fmt.Errorf("failed to delete payment vault: %w", err)
	}

	if err := s.PaymentMethodService.Delete(ctx, pm.ID); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to delete vault locally")
		return fmt.Errorf("failed to delete local vault record: %w", err)
	}

	log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully deleted payment vault")
	return nil
}

// ResolveClientForPaymentMethod resolves the per-merchant NMI client for the
// payment method's rail + declared rail merchant account — the surface the
// nmi_vault_delete intent handler executes through.
func (s *VaultService) ResolveClientForPaymentMethod(ctx context.Context, pm *models.PaymentMethod) (*nmi.NMIClient, error) {
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return nil, errors.New("payment method rail is required")
	}
	client, _, err := s.resolveNMIClient(ctx, rail, pm.RailMerchantAccountID)
	return client, err
}

// GetUserVaults lists all vaults for a user
func (s *VaultService) GetUserVaults(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	return s.PaymentMethodService.GetByUserID(ctx, userID)
}
