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

type RailPaymentMethodService struct {
	PaymentMethodService paymentMethodStore
	SubscriptionService  subscriptionReader
	MerchantSecrets      merchants.MerchantSecretReader
	// NMIEndpointOverride points store-armed NMI clients at a fake gateway
	// (test seam; empty = real endpoints).
	NMIEndpointOverride string
	ProviderSecrets     merchants.PSPSecretResolver
	ProviderScopes      merchants.PSPScopeResolver
	Config              *config.Config
	DB                  *db.DB
	// DeleteIntents routes DeletePaymentMethod through the durable nmi_vault_delete
	// provider intent (#674 tail); wired at runtime assembly.
	DeleteIntents PaymentMethodDeleteExecutor
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
func (s *RailPaymentMethodService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *RailPaymentMethodService) SetMerchantSecretStore(store merchants.MerchantSecretReader) {
	if s == nil {
		return
	}
	s.MerchantSecrets = store
}

func (s *RailPaymentMethodService) SetRailMerchantAccountSecretResolver(resolver merchants.PSPSecretResolver) {
	if s == nil {
		return
	}
	s.ProviderSecrets = resolver
	if scopes, ok := resolver.(merchants.PSPScopeResolver); ok {
		s.ProviderScopes = scopes
	}
}

func (s *RailPaymentMethodService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *RailPaymentMethodService) Clock() clockwork.Clock {
	return s.clock
}

type CreatePaymentMethodRequest struct {
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

type UpdatePaymentMethodRequest struct {
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

// PaymentMethodError carries additional context for payment method creation failures, including localization codes.
type PaymentMethodError struct {
	Err            error
	LocalizationID string
	Message        string
}

func (e *PaymentMethodError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "payment method error"
}

func (e *PaymentMethodError) Unwrap() error {
	return e.Err
}

func NewRailPaymentMethodService(pm *PaymentMethodService, sub subscriptionReader, dbx *db.DB, cfg *config.Config, clocks ...clockwork.Clock) *RailPaymentMethodService {
	return &RailPaymentMethodService{
		PaymentMethodService: pm,
		SubscriptionService:  sub,
		Config:               cfg,
		DB:                   dbx,
		clock:                timeutil.FirstClock(clocks...),
	}
}

// CreatePaymentMethod creates a NMI customer vault and stores a local PaymentMethod.
// req.Provider names the PSP (account key, e.g. "mobius") or a bare rail — the
// vault is created IN that PSP's account, and the row's rail comes from the
// resolved scope.
func (s *RailPaymentMethodService) CreatePaymentMethod(ctx context.Context, userID string, req *CreatePaymentMethodRequest) (*models.PaymentMethod, error) {
	psp := strings.TrimSpace(strings.ToLower(req.Provider))
	if psp == "" {
		return nil, errors.New("provider is required")
	}

	// or#896: refuse an unsupported RAIL honestly before credential resolution.
	// A reserved gateway name (stripe/ccbill/solana) resolves to itself, so the
	// old code reported "PSP 'stripe' is not configured" — a misconfiguration
	// message for a surface that does not exist on that rail.
	if rail := models.Rail(psp); knownRail(rail) && !rails.SupportsPaymentMethodCRUD(rail) {
		return nil, RailPaymentMethodsUnsupported(psp)
	}

	client, scope, err := s.resolveNMIClientByName(ctx, psp)
	if err != nil {
		if errors.Is(err, ErrPaymentMethodsUnsupportedOnRail) {
			return nil, err
		}
		return nil, fmt.Errorf("PSP '%s' is not configured: %w", psp, err)
	}
	rail := psp
	var pspID *uuid.UUID
	if scope != nil {
		rail = scope.Rail
		if scope.ID != uuid.Nil {
			id := scope.ID
			pspID = &id
		}
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

	nmiResponse, err := client.CreateCustomerVault(ctx, vaultData)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID}).Error("Failed to create vault in NMI")
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			return nil, &PaymentMethodError{
				Err:            err,
				LocalizationID: nmiErr.LocalizationID,
				Message:        fmt.Sprintf("failed to create payment method: %s", err.Error()),
			}
		}
		return nil, fmt.Errorf("failed to create payment method: %w", err)
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
	if pspID != nil {
		pm.PspID = pspID
	}

	if err := s.PaymentMethodService.Create(ctx, pm); err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID, "vault_id": nmiResponse.CustomerVaultID}).Error("Failed to store payment method locally")
		// Best-effort direct remote cleanup — deliberately NOT intent-routed
		// (#674 tail): the vault was created milliseconds ago and is referenced
		// nowhere; losing this delete leaves only an inert orphan entry at NMI.
		_ = client.DeleteCustomerVault(ctx, nmi.DeleteCustomerVaultData{CustomerVaultID: nmiResponse.CustomerVaultID})
		return nil, fmt.Errorf("failed to store payment method locally: %w", err)
	}

	log.WithFields(log.Fields{"user_id": userID, "vault_id": pm.RailCustomerRef}).Info("Successfully created payment method")
	return pm, nil
}

func (s *RailPaymentMethodService) resolveNMIClient(ctx context.Context, provider string, pspID ...*uuid.UUID) (*nmi.NMIClient, *uuid.UUID, error) {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return nil, nil, errors.New("rail is required")
	}

	if len(pspID) > 0 && pspID[0] != nil {
		if s == nil || s.DB == nil {
			return nil, nil, errors.New("provider account lookup unavailable")
		}
		row, err := s.DB.Gen(ctx).GetPSP(ctx, *pspID[0])
		if err != nil {
			return nil, nil, err
		}
		if !rails.SameRail(models.Rail(row.Rail), models.Rail(provider)) {
			return nil, nil, fmt.Errorf("provider account %s belongs to rail %s, not %s", row.ID, row.Rail, provider)
		}
		client, err := s.resolveNMIClientForScope(ctx, merchants.PSPScope{
			ID:          row.ID,
			Rail:        row.Rail,
			Environment: row.Environment,
			AccountID:   row.AccountID,
		})
		return client, &row.ID, err
	}

	var scopeResolver merchants.PSPScopeResolver
	if s != nil {
		scopeResolver = s.ProviderScopes
		if scopeResolver == nil {
			if resolver, ok := s.ProviderSecrets.(merchants.PSPScopeResolver); ok {
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
		scope, ok, err := scopeResolver.ActivePSPScope(ctx, tid, string(models.RailNMI), env)
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

	return nil, nil, errors.New("missing client")
}

// resolveNMIClientByName resolves the client for a PSP key or a bare rail
// name: a declared PSP key pins its own account; a rail resolves to the
// active armed account. Returns the resolved scope (nil when only static
// resolution was possible).
func (s *RailPaymentMethodService) resolveNMIClientByName(ctx context.Context, name string) (*nmi.NMIClient, *merchants.PSPScope, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, nil, errors.New("psp is required")
	}
	var keyResolver merchants.PSPKeyResolver
	if s != nil {
		if kr, ok := s.ProviderScopes.(merchants.PSPKeyResolver); ok {
			keyResolver = kr
		} else if kr, ok := s.ProviderSecrets.(merchants.PSPKeyResolver); ok {
			keyResolver = kr
		}
	}
	if keyResolver != nil {
		if tid, err := merchant.Require(ctx); err == nil {
			env := config.ExpectedProviderEnvironment(s.Config != nil && s.Config.IsTestMode())
			scope, found, err := keyResolver.PSPScopeByKey(ctx, tid, name, env)
			if err != nil {
				return nil, nil, fmt.Errorf("resolve PSP %q: %w", name, err)
			}
			if found {
				if !rails.SupportsPaymentMethodCRUD(models.Rail(scope.Rail)) {
					// or#896: honest unsupported-surface refusal, not a
					// credential complaint about a PSP that resolved fine.
					return nil, nil, RailPaymentMethodsUnsupported(scope.Rail)
				}
				client, err := s.resolveNMIClientForScope(ctx, scope)
				if err != nil {
					return nil, nil, err
				}
				return client, &scope, nil
			}
		}
	}
	client, pspID, err := s.resolveNMIClient(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if pspID != nil {
		return client, &merchants.PSPScope{ID: *pspID, Rail: name}, nil
	}
	return client, nil, nil
}

func (s *RailPaymentMethodService) resolveNMIClientForScope(ctx context.Context, scope merchants.PSPScope) (*nmi.NMIClient, error) {
	provider := strings.TrimSpace(scope.AccountID)
	if provider == "" {
		return nil, errors.New("provider account_id required")
	}
	if s == nil || s.MerchantSecrets == nil {
		return nil, errors.New("missing scoped merchant NMI secret for provider account")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	secretName, err := merchants.PSPSecretName(scope.Rail, scope.Environment, scope.AccountID, "security_key")
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
	proc := &config.PSPConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{SecurityKey: value}}
	return s.buildNMIClient(provider, proc.ToNMIProviderSettings())
}

func (s *RailPaymentMethodService) buildNMIClient(provider string, cfg *config.NMIProviderSettings) (*nmi.NMIClient, error) {
	testMode := s != nil && s.Config != nil && s.Config.IsTestMode()
	if s != nil && s.newNMIClient != nil {
		return s.newNMIClient(provider, cfg, testMode)
	}
	client, err := nmi.NewClient(provider, cfg, testMode)
	if err != nil {
		return nil, err
	}
	if s != nil && s.NMIEndpointOverride != "" {
		client.DirectPostURL = s.NMIEndpointOverride
		client.QueryURL = s.NMIEndpointOverride
		client.V5BaseURL = s.NMIEndpointOverride
	}
	return client, nil
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

// UpdatePaymentMethod updates vault in NMI and updates local record timestamp
func (s *RailPaymentMethodService) UpdatePaymentMethod(ctx context.Context, pm *models.PaymentMethod, req *UpdatePaymentMethodRequest) (*models.PaymentMethod, error) {
	// Use rail from the payment method.
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return nil, errors.New("payment method rail is required")
	}

	if !rails.SupportsPaymentMethodCRUD(models.Rail(rail)) {
		return nil, RailPaymentMethodsUnsupported(rail)
	}

	client, _, err := s.resolveNMIClient(ctx, rail, pm.PspID)
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

	if err := client.UpdateCustomerVault(ctx, upd); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to update vault in NMI")
		return nil, fmt.Errorf("failed to update payment method: %w", err)
	}

	if paymentTokenUpdated {
		applyUpdatedCardMetadata(pm, req)
	}
	pm.UpdatedAt = s.now()
	if err := s.PaymentMethodService.Update(ctx, pm); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to update local payment method record")
		return nil, fmt.Errorf("failed to update local payment method record: %w", err)
	}
	log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully updated payment method")
	return pm, nil
}

func applyUpdatedCardMetadata(pm *models.PaymentMethod, req *UpdatePaymentMethodRequest) {
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

// ErrPaymentMethodDeleteProcessing: the durable vault-delete intent could not confirm
// the remote delete inline (transport-ambiguous outcome, parked provider). The
// intent ledger completes it out-of-band; a retried request maps onto the SAME
// intent. Never a lost delete.
var ErrPaymentMethodDeleteProcessing = errors.New("payment method deletion is processing; it will complete automatically")

// PaymentMethodDeleteOutcome mirrors the durable intent's post-execution state without
// importing the intents package (import cycle: intents → subscriptions →
// paymentmethods). Neither Done nor Terminal = still resolving out-of-band.
type PaymentMethodDeleteOutcome struct {
	// Done: the remote delete is confirmed and the local row is gone.
	Done bool
	// Terminal: the delete failed permanently; Reason says why.
	Terminal bool
	Reason   string
}

// PaymentMethodDeleteExecutor posts the durable nmi_vault_delete intent and executes
// it inline (#674 write-through). Implemented by intents.PaymentMethodDeleteThrough.
type PaymentMethodDeleteExecutor interface {
	ExecutePaymentMethodDelete(ctx context.Context, pm *models.PaymentMethod) (PaymentMethodDeleteOutcome, error)
}

// DeletePaymentMethod deletes a stored payment method the DURABLE way (#674 tail):
// guards run inline, then the remote NMI delete goes through the write-ahead
// nmi_vault_delete intent — a crash or lost response after the user's request
// is accepted can never lose the delete (the verifier resolves "vault gone at
// provider ⇒ done" and finalizes the local removal).
func (s *RailPaymentMethodService) DeletePaymentMethod(ctx context.Context, pm *models.PaymentMethod) error {
	if _, _, err := s.deletePaymentMethodGuards(ctx, pm); err != nil {
		return err
	}
	if s.DeleteIntents == nil {
		return errors.New("vault delete intent executor not wired")
	}
	out, err := s.DeleteIntents.ExecutePaymentMethodDelete(ctx, pm)
	if err != nil {
		return fmt.Errorf("post vault delete intent: %w", err)
	}
	switch {
	case out.Done:
		log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully deleted payment method")
		return nil
	case out.Terminal:
		return fmt.Errorf("failed to delete payment method: %s", out.Reason)
	default:
		return ErrPaymentMethodDeleteProcessing
	}
}

// CleanupPaymentMethodBestEffort is the DIRECT delete for reactive decline-cleanup
// (checkout removing a vault it just created for a now-declined attempt).
// Deliberately NOT intent-routed (#674 tail): the vault is referenced nowhere,
// a lost delete leaves only an inert orphan entry at NMI (no billing state can
// act on it), and card-testing decline floods must not spam the intent ledger
// or burn the #679 destructive-volume budget. Durable user-initiated deletes
// go through DeletePaymentMethod.
func (s *RailPaymentMethodService) CleanupPaymentMethodBestEffort(ctx context.Context, pm *models.PaymentMethod) error {
	shared, client, err := s.deletePaymentMethodGuards(ctx, pm)
	if err != nil {
		return err
	}
	return s.deletePaymentMethodDirect(ctx, client, pm, shared)
}

// deletePaymentMethodGuards runs the shared pre-delete checks: no active subscription
// may use the method, the rail must resolve to a configured client (fail fast
// instead of parking a user-facing request), and a shared vault without a
// billing id is refused outright.
func (s *RailPaymentMethodService) deletePaymentMethodGuards(ctx context.Context, pm *models.PaymentMethod) (shared bool, client *nmi.NMIClient, err error) {
	subs, _, err := s.SubscriptionService.GetPaginatedByUserID(ctx, pm.CustomerID.String(), 1, 1000)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"vault_id": pm.RailCustomerRef, "user_id": pm.CustomerID.String()}).Error("Failed to check subscriptions for payment method")
		return false, nil, fmt.Errorf("failed to check payment method usage: %w", err)
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
		return false, nil, fmt.Errorf("cannot delete payment method: %d active subscription(s) are using this payment method", activeCount)
	}

	// Use rail from the payment method
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return false, nil, errors.New("payment method rail is required")
	}

	if !rails.SupportsPaymentMethodCRUD(models.Rail(rail)) {
		return false, nil, RailPaymentMethodsUnsupported(rail)
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
			return false, nil, fmt.Errorf("failed to check payment method sharing: %w", err)
		}
		shared = n > 0
	}
	if shared && strings.TrimSpace(pm.RailMethodRef) == "" {
		// Cannot identify WHICH entry is this card — refuse rather than
		// destroy the siblings (an importer that shares vaults must record
		// per-row billing ids, see #682's shared-vault contract).
		return shared, nil, fmt.Errorf("cannot delete vault %s: shared by other stored payment methods and this row carries no billing id to scope the delete", pm.RailCustomerRef)
	}

	client, _, err = s.resolveNMIClient(ctx, rail, pm.PspID)
	if err != nil {
		return shared, nil, fmt.Errorf("rail '%s' is not configured: %w", rail, err)
	}
	return shared, client, nil
}

// deletePaymentMethodDirect performs the remote delete (billing-entry-scoped for
// shared vaults, whole-vault otherwise) followed by the local removal.
func (s *RailPaymentMethodService) deletePaymentMethodDirect(ctx context.Context, client *nmi.NMIClient, pm *models.PaymentMethod, shared bool) error {
	if shared {
		if err := client.DeleteCustomerBillingEntry(ctx, pm.RailCustomerRef, pm.RailMethodRef); err != nil {
			log.WithError(err).WithFields(log.Fields{"vault_id": pm.RailCustomerRef, "billing_id": pm.RailMethodRef}).Error("Failed to delete vault billing entry from NMI")
			return fmt.Errorf("failed to delete payment method entry: %w", err)
		}
		return s.PaymentMethodService.Delete(ctx, pm.ID)
	}

	if err := client.DeleteCustomerVault(ctx, nmi.DeleteCustomerVaultData{CustomerVaultID: pm.RailCustomerRef}); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to delete vault from NMI")
		return fmt.Errorf("failed to delete payment method: %w", err)
	}

	if err := s.PaymentMethodService.Delete(ctx, pm.ID); err != nil {
		log.WithError(err).WithField("vault_id", pm.RailCustomerRef).Error("Failed to delete vault locally")
		return fmt.Errorf("failed to delete local vault record: %w", err)
	}

	log.WithField("vault_id", pm.RailCustomerRef).Info("Successfully deleted payment method")
	return nil
}

// ResolveClientForPaymentMethod resolves the per-merchant NMI client for the
// payment method's rail + declared rail merchant account — the surface the
// nmi_vault_delete intent handler executes through.
func (s *RailPaymentMethodService) ResolveClientForPaymentMethod(ctx context.Context, pm *models.PaymentMethod) (*nmi.NMIClient, error) {
	rail := strings.ToLower(string(pm.Rail))
	if rail == "" {
		return nil, errors.New("payment method rail is required")
	}
	client, _, err := s.resolveNMIClient(ctx, rail, pm.PspID)
	return client, err
}

// GetUserPaymentMethods lists all vaults for a user
func (s *RailPaymentMethodService) GetUserPaymentMethods(ctx context.Context, userID string) ([]*models.PaymentMethod, error) {
	return s.PaymentMethodService.GetByUserID(ctx, userID)
}
