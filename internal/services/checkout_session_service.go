package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/pkg/api"
)

const (
	checkoutSessionIdempotencyOp = "checkout_session_create"
	defaultCheckoutSessionTTL    = 15 * time.Minute
	redirectCheckoutSessionTTL   = 24 * time.Hour
)

var (
	ErrCheckoutSessionValidation       = errors.New("checkout session validation failed")
	ErrCheckoutSessionNotFound         = errors.New("checkout session not found")
	ErrCheckoutSessionForbidden        = errors.New("checkout session access denied")
	ErrCheckoutSessionExpired          = errors.New("checkout session expired")
	ErrCheckoutSessionPending          = errors.New("checkout session request already pending")
	ErrCheckoutSessionConflict         = errors.New("checkout session conflict")
	ErrCheckoutSessionNotSolana        = errors.New("checkout session is not a solana session")
	ErrCheckoutSessionAlreadyCompleted = errors.New("checkout session already completed")
)

type CheckoutSessionPaymentRequest struct {
	Processor       string
	PaymentMethodID string
	PaymentToken    string

	TokenSymbol string
	Flow        string
	Wallet      string

	Email      string
	FirstName  string
	LastName   string
	Address1   string
	City       string
	State      string
	Zip        string
	Country    string
	LastFour   string
	CardType   string
	ExpiryDate string
}

type CheckoutSessionCreateRequest struct {
	PriceID        string
	Mode           string
	Payment        CheckoutSessionPaymentRequest
	Metadata       map[string]string
	IdempotencyKey string
}

type CheckoutSessionConfirmPayment struct {
	Processor string
	Signature string
	Wallet    string
}

type CheckoutSessionConfirmRequest struct {
	Payment CheckoutSessionConfirmPayment
}

type CheckoutSessionRedirectToURL struct {
	URL       string `json:"url,omitempty"`
	ReturnURL string `json:"return_url,omitempty"`
}

type CheckoutSessionNextAction struct {
	Type          string                        `json:"type"`
	RedirectToURL *CheckoutSessionRedirectToURL `json:"redirect_to_url,omitempty"`
}

type CheckoutSessionPaymentResponse struct {
	Processor      string `json:"processor"`
	Reference      string `json:"reference,omitempty"`
	TransactionURL string `json:"transaction_url,omitempty"` // For transfer_request flow (solana: URL)
	SolanaPayURL   string `json:"solana_pay_url,omitempty"`  // For transaction_request flow (solana:https:// URL)
	RedirectURL    string `json:"redirect_url,omitempty"`
	TransactionID  string `json:"transaction_id,omitempty"`
}

type CheckoutSessionResponse struct {
	Object         string                         `json:"object"`
	ID             string                         `json:"id"`
	Status         string                         `json:"status"`
	Mode           string                         `json:"mode"`
	PriceID        string                         `json:"price_id"`
	Payment        CheckoutSessionPaymentResponse `json:"payment"`
	PaymentID      *string                        `json:"payment_id,omitempty"`
	SubscriptionID *string                        `json:"subscription_id,omitempty"`
	ExpiresAt      *time.Time                     `json:"expires_at,omitempty"`
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`
	Message        string                         `json:"message,omitempty"`
	Metadata       map[string]string              `json:"metadata,omitempty"`
}

type CheckoutSessionService struct {
	db                       *db.DB
	repo                     *repo.CheckoutSessionRepo
	priceService             *PriceService
	productService           *ProductService
	paymentMethodService     *PaymentMethodService
	idempotencyService       *IdempotencyService
	checkoutService          *CheckoutService
	solanaPayService         *SolanaPayService
	solanaTransactionService *SolanaTransactionService
	fxProvider               fx.Provider
	config                   *config.Config
	Clock                    clockwork.Clock
}

func NewCheckoutSessionService(
	db *db.DB,
	priceService *PriceService,
	productService *ProductService,
	paymentMethodService *PaymentMethodService,
	idempotencyService *IdempotencyService,
	checkoutService *CheckoutService,
	solanaPayService *SolanaPayService,
	solanaTransactionService *SolanaTransactionService,
	fxProvider fx.Provider,
	cfg *config.Config,
) *CheckoutSessionService {
	return &CheckoutSessionService{
		db:                       db,
		repo:                     repo.NewCheckoutSessionRepo(db),
		priceService:             priceService,
		productService:           productService,
		paymentMethodService:     paymentMethodService,
		idempotencyService:       idempotencyService,
		checkoutService:          checkoutService,
		solanaPayService:         solanaPayService,
		solanaTransactionService: solanaTransactionService,
		fxProvider:               fxProvider,
		config:                   cfg,
	}
}

func (s *CheckoutSessionService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *CheckoutSessionService) CreateSession(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return nil, fmt.Errorf("%w: user is required", ErrCheckoutSessionValidation)
	}
	if req == nil {
		return nil, fmt.Errorf("%w: request is required", ErrCheckoutSessionValidation)
	}

	req.IdempotencyKey = scopeIdempotencyKey(user.ID, req.IdempotencyKey)

	claimed := false
	if s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		rec, exists, err := s.idempotencyService.Begin(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if exists {
			switch rec.Status {
			case IdempotencyStatusSuccess:
				var cached CheckoutSessionResponse
				if err := json.Unmarshal(rec.Result, &cached); err != nil {
					return nil, fmt.Errorf("failed to decode cached response: %w", err)
				}
				return &cached, nil
			case IdempotencyStatusPending:
				return nil, ErrCheckoutSessionPending
			case IdempotencyStatusFailed:
				return nil, fmt.Errorf("%w: previous request failed: %s", ErrCheckoutSessionConflict, rec.Error)
			}
		}
		claimed = true
	}

	resp, err := s.createSessionWithValidation(ctx, req, user)
	if err != nil {
		if claimed && s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
			_ = s.idempotencyService.Fail(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey, err)
		}
		return nil, err
	}

	if claimed && s.idempotencyService != nil && strings.TrimSpace(req.IdempotencyKey) != "" {
		payload, _ := json.Marshal(resp)
		_ = s.idempotencyService.Complete(ctx, checkoutSessionIdempotencyOp, req.IdempotencyKey, payload)
	}

	return resp, nil
}

func (s *CheckoutSessionService) createSessionWithValidation(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if strings.TrimSpace(req.PriceID) == "" {
		return nil, fmt.Errorf("%w: price_id is required", ErrCheckoutSessionValidation)
	}
	if strings.TrimSpace(req.Payment.Processor) == "" {
		return nil, fmt.Errorf("%w: payment.processor is required", ErrCheckoutSessionValidation)
	}

	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid price_id", ErrCheckoutSessionValidation)
	}
	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("%w: price not found", ErrCheckoutSessionValidation)
	}
	if !price.IsActive {
		return nil, fmt.Errorf("%w: price is not active", ErrCheckoutSessionValidation)
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("%w: product not found", ErrCheckoutSessionValidation)
	}
	if !product.IsActive {
		return nil, fmt.Errorf("%w: product is not active", ErrCheckoutSessionValidation)
	}

	processor := strings.ToLower(strings.TrimSpace(req.Payment.Processor))
	mode, err := s.resolveMode(req.Mode, processor, price)
	if err != nil {
		return nil, err
	}

	if err := s.validatePayment(ctx, processor, &req.Payment, user); err != nil {
		return nil, fmt.Errorf("error validating payment: %s", err)
	}

	now := s.now()
	ttl := defaultCheckoutSessionTTL
	if processor == "ccbill" || processor == "stripe" {
		ttl = redirectCheckoutSessionTTL
	}
	session := &models.CheckoutSession{
		ID:              uuid.New(),
		UserID:          user.ID,
		PriceID:         price.ID,
		Mode:            mode,
		Processor:       models.Processor(processor),
		Status:          models.CheckoutSessionStatusCreated,
		Amount:          price.Amount,
		Currency:        price.Currency,
		ExpiresAt:       timePtr(now.Add(ttl)),
		Metadata:        normalizeMetadata(req.Metadata),
		ProcessorFields: s.buildProcessorFields(processor, &req.Payment),
		ProcessorState:  map[string]any{},
		CreatedAt:       now,
		UpdatedAt:       now,
		LastFour:        &req.Payment.LastFour,
		CardType:        &req.Payment.CardType,
		ExpiryDate:      &req.Payment.ExpiryDate,
	}

	if strings.TrimSpace(req.IdempotencyKey) != "" {
		session.IdempotencyKey = stringPtr(strings.TrimSpace(req.IdempotencyKey))
	}

	if err := s.repo.Create(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to create checkout session: %w", err)
	}

	if err := s.initializeSession(ctx, session, &req.Payment, user); err != nil {
		_ = s.MarkFailed(ctx, session.ID, err.Error(), "")
		return nil, err
	}

	session.UpdatedAt = s.now()
	if err := s.repo.Update(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to update checkout session: %w", err)
	}

	return s.sessionToResponse(session), nil
}

func (s *CheckoutSessionService) GetSession(ctx context.Context, sessionID uuid.UUID, user *UserIdentity) (*CheckoutSessionResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.UserID != user.ID {
		return nil, ErrCheckoutSessionForbidden
	}

	if s.isExpired(session) && !s.isTerminal(session.Status) {
		session.Status = models.CheckoutSessionStatusExpired
		session.UpdatedAt = s.now()
		if updateErr := s.repo.Update(ctx, session); updateErr != nil {
			return nil, fmt.Errorf("failed to update expired session: %w", updateErr)
		}
	}

	return s.sessionToResponse(session), nil
}

func (s *CheckoutSessionService) ConfirmSession(ctx context.Context, sessionID uuid.UUID, req *CheckoutSessionConfirmRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if user == nil || strings.TrimSpace(user.ID) == "" || session.UserID != user.ID {
		return nil, ErrCheckoutSessionForbidden
	}

	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			return s.sessionToResponse(session), nil
		}
		if session.Status != models.CheckoutSessionStatusExpired {
			return nil, ErrCheckoutSessionConflict
		}
	}
	if s.isExpired(session) {
		if !s.isTerminal(session.Status) {
			_ = s.MarkExpired(ctx, session.ID, "checkout session expired")
		}
		return nil, ErrCheckoutSessionExpired
	}

	processor := strings.ToLower(strings.TrimSpace(req.Payment.Processor))
	if processor == "" {
		return nil, fmt.Errorf("%w: payment.processor is required", ErrCheckoutSessionValidation)
	}
	if processor != strings.ToLower(string(session.Processor)) {
		return nil, fmt.Errorf("%w: processor mismatch", ErrCheckoutSessionValidation)
	}

	switch processor {
	case "solana":
		return s.confirmSolanaSession(ctx, session, req, user)
	default:
		return nil, fmt.Errorf("%w: confirmation not implemented for processor %s", ErrCheckoutSessionConflict, processor)
	}
}

func (s *CheckoutSessionService) resolveMode(mode string, processor string, price *models.Price) (models.CheckoutSessionMode, error) {
	if processor == "" {
		return "", fmt.Errorf("%w: processor is required", ErrCheckoutSessionValidation)
	}

	trimmedMode := strings.TrimSpace(mode)
	if processor == "solana" {
		if trimmedMode == string(models.CheckoutSessionModeSubscription) {
			return "", fmt.Errorf("%w: solana does not support subscription mode", ErrCheckoutSessionValidation)
		}
		return models.CheckoutSessionModeOneOff, nil
	}

	expected := models.CheckoutSessionModeOneOff
	if price.BillingCycleDays != nil {
		expected = models.CheckoutSessionModeSubscription
	}
	if trimmedMode == "" {
		return expected, nil
	}
	if trimmedMode != string(expected) {
		return "", fmt.Errorf("%w: mode does not match price configuration", ErrCheckoutSessionValidation)
	}
	return models.CheckoutSessionMode(trimmedMode), nil
}

func (s *CheckoutSessionService) validatePayment(ctx context.Context, processor string, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	// Route to processor-specific validation based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case processors.IsNMIBacked(processor):
		hasToken := strings.TrimSpace(payment.PaymentToken) != ""
		hasMethod := strings.TrimSpace(payment.PaymentMethodID) != ""
		if hasToken == hasMethod {
			return fmt.Errorf("%w: provide either payment_token or payment_method_id", ErrCheckoutSessionValidation)
		}
		if hasMethod {
			pmID, err := api.ParsePaymentMethodID(payment.PaymentMethodID)
			if err != nil {
				return fmt.Errorf("%w: invalid payment_method_id", ErrCheckoutSessionValidation)
			}
			if s.paymentMethodService == nil {
				return fmt.Errorf("%w: payment method service unavailable", ErrCheckoutSessionValidation)
			}
			if err := s.paymentMethodService.ValidateOwnership(ctx, pmID, user.ID); err != nil {
				return fmt.Errorf("%w: payment method not authorized", ErrCheckoutSessionValidation)
			}
		}
	case processor == "stripe":
		hasToken := strings.TrimSpace(payment.PaymentToken) != ""
		hasMethod := strings.TrimSpace(payment.PaymentMethodID) != ""
		if hasToken && hasMethod {
			return fmt.Errorf("%w: provide either payment_token or payment_method_id", ErrCheckoutSessionValidation)
		}
		if hasMethod {
			pmID, err := api.ParsePaymentMethodID(payment.PaymentMethodID)
			if err != nil {
				return fmt.Errorf("%w: invalid payment_method_id", ErrCheckoutSessionValidation)
			}
			if s.paymentMethodService == nil {
				return fmt.Errorf("%w: payment method service unavailable", ErrCheckoutSessionValidation)
			}
			if err := s.paymentMethodService.ValidateOwnership(ctx, pmID, user.ID); err != nil {
				return fmt.Errorf("%w: payment method not authorized", ErrCheckoutSessionValidation)
			}
		}
		if err := requireBillingFields(payment); err != nil {
			return err
		}
	case processor == "solana":
		if strings.TrimSpace(payment.TokenSymbol) == "" {
			return fmt.Errorf("%w: token_symbol is required", ErrCheckoutSessionValidation)
		}
	case processor == "ccbill":
		if err := requireBillingFields(payment); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported processor", ErrCheckoutSessionValidation)
	}
	return nil
}

func (s *CheckoutSessionService) initializeSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	if payment == nil {
		return fmt.Errorf("%w: payment is required", ErrCheckoutSessionValidation)
	}

	processor := strings.ToLower(string(session.Processor))
	// Route to processor-specific initialization based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case processor == "solana":
		return s.initializeSolanaSession(ctx, session, payment)
	case processors.IsNMIBacked(processor):
		return s.initializeCheckoutSession(ctx, session, payment, user)
	case processor == "ccbill" || processor == "stripe":
		return s.initializeCheckoutSession(ctx, session, payment, user)
	default:
		return nil
	}
}

func (s *CheckoutSessionService) initializeSolanaSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest) error {
	solanaProc := s.config.GetSolanaProcessor()
	if s.config == nil || solanaProc == nil {
		return fmt.Errorf("%w: solana not configured", ErrCheckoutSessionValidation)
	}

	tokenSymbol := strings.TrimSpace(payment.TokenSymbol)
	if tokenSymbol == "" {
		return fmt.Errorf("%w: token_symbol is required", ErrCheckoutSessionValidation)
	}

	flow := strings.TrimSpace(payment.Flow)
	if flow == "" {
		flow = "transfer_request"
	}

	tokenCfg, ok := solanaProc.SupportedTokens[tokenSymbol]
	if !ok || !tokenCfg.Enabled {
		return fmt.Errorf("%w: unsupported token", ErrCheckoutSessionValidation)
	}
	tokenMint := tokenCfg.Mint
	if strings.EqualFold(solanaProc.Network, "mainnet") && tokenCfg.MainnetMint != "" {
		tokenMint = tokenCfg.MainnetMint
	}

	switch flow {
	case "transfer_request":
		if s.solanaPayService == nil {
			return fmt.Errorf("%w: solana pay service unavailable", ErrCheckoutSessionValidation)
		}
		result, err := s.solanaPayService.GeneratePayment(ctx, session.UserID, session.PriceID, tokenSymbol, &session.ID)
		if err != nil {
			return err
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		session.Reference = &result.Reference
		session.ExpiresAt = &result.ExpiresAt
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["transaction_url"] = result.URL
		session.ProcessorState["flow"] = flow
		session.ProcessorState["token_symbol"] = tokenSymbol
		tokenMintValue := strings.TrimSpace(result.TokenMint)
		if tokenMintValue == "" {
			tokenMintValue = tokenMint
		}
		recipient := strings.TrimSpace(result.Recipient)
		if recipient == "" {
			return fmt.Errorf("%w: recipient missing from payment quote", ErrCheckoutSessionValidation)
		}
		session.ProcessorState["token_mint"] = tokenMintValue
		session.ProcessorState["recipient"] = recipient
		if err := setSolanaQuoteState(session.ProcessorState, result.TokenUnits, result.TokenPriceUSD, result.FXRate, result.FXCurrency, result.QuotedAt, result.QuoteExpiresAt); err != nil {
			return err
		}
	case "transaction_request":
		// Transaction Request flow per Solana Pay spec:
		// - Wallet address is NOT required at session creation
		// - Transaction is built later when wallet calls POST /v1/checkout/:id/solana-pay
		// - Session just stores flow and token info, returns solana_pay_url for wallet
		if s.solanaTransactionService == nil {
			return fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		expiresAt := s.now().Add(defaultCheckoutSessionTTL)
		session.ExpiresAt = &expiresAt
		recipient := strings.TrimSpace(solanaProc.RecipientWallet)
		if recipient == "" {
			return fmt.Errorf("%w: recipient wallet not configured", ErrCheckoutSessionValidation)
		}
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["flow"] = flow
		session.ProcessorState["token_symbol"] = tokenSymbol
		session.ProcessorState["token_mint"] = tokenMint
		session.ProcessorState["recipient"] = recipient
		quote, err := CalculateTokenQuote(ctx, tokenCfg, session.Amount, session.Currency, s.fxProvider)
		if err != nil {
			return fmt.Errorf("%w: failed to calculate solana token quote: %v", ErrCheckoutSessionValidation, err)
		}
		if err := setSolanaQuoteState(session.ProcessorState, quote.Units, quote.TokenPriceUSD, quote.FXRate, quote.FXCurrency, quote.QuotedAt, expiresAt); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported solana flow", ErrCheckoutSessionValidation)
	}

	return nil
}

func (s *CheckoutSessionService) initializeCheckoutSession(ctx context.Context, session *models.CheckoutSession, payment *CheckoutSessionPaymentRequest, user *UserIdentity) error {
	if s.checkoutService == nil {
		return fmt.Errorf("%w: checkout service unavailable", ErrCheckoutSessionValidation)
	}

	req := &CheckoutRequest{
		PriceID:         api.FormatPriceID(session.PriceID),
		PaymentMethodID: payment.PaymentMethodID,
		PaymentToken:    payment.PaymentToken,
		Processor:       string(session.Processor),
		Metadata:        session.Metadata,
		Email:           payment.Email,
		FirstName:       payment.FirstName,
		LastName:        payment.LastName,
		Address1:        payment.Address1,
		City:            payment.City,
		State:           payment.State,
		Zip:             payment.Zip,
		Country:         payment.Country,
		LastFour:        payment.LastFour,
		CardType:        payment.CardType,
		ExpiryDate:      payment.ExpiryDate,
	}

	if session.IdempotencyKey != nil {
		req.IdempotencyKey = strings.TrimSpace(*session.IdempotencyKey)
	}
	if session.Processor == models.ProcessorStripe {
		req.CheckoutSessionID = api.FormatCheckoutSessionID(session.ID)
	}

	resp, err := s.checkoutService.Checkout(ctx, req, user)
	if err != nil {
		return err
	}

	return s.applyCheckoutResponse(session, resp)
}

func (s *CheckoutSessionService) applyCheckoutResponse(session *models.CheckoutSession, resp *CheckoutResponse) error {
	if session == nil {
		return fmt.Errorf("%w: session is required", ErrCheckoutSessionValidation)
	}
	if resp == nil {
		return fmt.Errorf("%w: checkout response is required", ErrCheckoutSessionValidation)
	}

	switch resp.Status {
	case "success", "pending":
		session.Status = models.CheckoutSessionStatusSucceeded
		if resp.PaymentID != nil {
			session.PaymentID = resp.PaymentID
		}
		if resp.SubscriptionID != nil {
			session.SubscriptionID = resp.SubscriptionID
		}
		if strings.TrimSpace(resp.TransactionID) != "" {
			session.TransactionID = stringPtr(resp.TransactionID)
		}
	case "redirect_required":
		redirectURL := strings.TrimSpace(resp.RedirectURL)
		if redirectURL == "" {
			return fmt.Errorf("%w: redirect url missing", ErrCheckoutSessionValidation)
		}
		session.Status = models.CheckoutSessionStatusRequiresAction
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["redirect_url"] = redirectURL
	case "blocked":
		msg := strings.TrimSpace(resp.Message)
		if msg == "" {
			msg = "checkout blocked"
		}
		return fmt.Errorf("%w: %s", ErrCheckoutSessionConflict, msg)
	default:
		return fmt.Errorf("%w: unsupported checkout status", ErrCheckoutSessionConflict)
	}

	return nil
}

func requireBillingFields(payment *CheckoutSessionPaymentRequest) error {
	if strings.TrimSpace(payment.Email) == "" ||
		strings.TrimSpace(payment.FirstName) == "" ||
		strings.TrimSpace(payment.LastName) == "" ||
		strings.TrimSpace(payment.Address1) == "" ||
		strings.TrimSpace(payment.City) == "" ||
		strings.TrimSpace(payment.Zip) == "" ||
		strings.TrimSpace(payment.Country) == "" {
		return fmt.Errorf("%w: billing fields are required", ErrCheckoutSessionValidation)
	}
	if _, err := mail.ParseAddress(strings.TrimSpace(payment.Email)); err != nil {
		return fmt.Errorf("%w: email is invalid", ErrCheckoutSessionValidation)
	}
	if len(strings.TrimSpace(payment.Country)) != 2 {
		return fmt.Errorf("%w: country must be ISO-3166 alpha-2", ErrCheckoutSessionValidation)
	}
	return nil
}

func (s *CheckoutSessionService) buildProcessorFields(processor string, payment *CheckoutSessionPaymentRequest) map[string]any {
	fields := map[string]any{
		"processor": processor,
	}

	addField(fields, "payment_method_id", payment.PaymentMethodID)
	addField(fields, "token_symbol", payment.TokenSymbol)
	addField(fields, "flow", payment.Flow)
	addField(fields, "wallet", payment.Wallet)
	addField(fields, "email", payment.Email)
	addField(fields, "first_name", payment.FirstName)
	addField(fields, "last_name", payment.LastName)
	addField(fields, "address1", payment.Address1)
	addField(fields, "city", payment.City)
	addField(fields, "state", payment.State)
	addField(fields, "zip", payment.Zip)
	addField(fields, "country", payment.Country)

	return fields
}

func addField(fields map[string]any, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	fields[key] = strings.TrimSpace(value)
}

func (s *CheckoutSessionService) sessionToResponse(session *models.CheckoutSession) *CheckoutSessionResponse {
	resp := &CheckoutSessionResponse{
		Object:  "checkout_session",
		ID:      api.FormatCheckoutSessionID(session.ID),
		Status:  string(session.Status),
		Mode:    string(session.Mode),
		PriceID: api.FormatPriceID(session.PriceID),
		Payment: CheckoutSessionPaymentResponse{
			Processor: string(session.Processor),
		},
		ExpiresAt: session.ExpiresAt,
	}
	if len(session.Metadata) > 0 {
		resp.Metadata = session.Metadata
	}

	if session.Reference != nil {
		resp.Payment.Reference = *session.Reference
	}
	if session.TransactionID != nil {
		resp.Payment.TransactionID = *session.TransactionID
	}

	if session.PaymentID != nil {
		paymentID := api.FormatPaymentID(*session.PaymentID)
		resp.PaymentID = &paymentID
	}
	if session.SubscriptionID != nil {
		subID := api.FormatSubscriptionID(*session.SubscriptionID)
		resp.SubscriptionID = &subID
	}

	if session.ProcessorState != nil {
		if val, ok := session.ProcessorState["transaction_url"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Payment.TransactionURL = val
		}
		// Build solana_pay_url for transaction_request flow
		if flow, ok := session.ProcessorState["flow"].(string); ok && flow == "transaction_request" {
			// Construct the Solana Pay URL:
			// - standalone: solana:{api_url}/v1/checkout/:id/solana-pay
			// - embedded:   solana:{api_url}/v1/checkout/:id/solana-pay (api_url typically ends with /billing)
			baseURL := s.getAPIBaseURL()
			if baseURL != "" {
				resp.Payment.SolanaPayURL = fmt.Sprintf(
					"solana:%s%s/checkout/%s/solana-pay",
					baseURL,
					s.getExternalV1Path(baseURL),
					api.FormatCheckoutSessionID(session.ID),
				)
			}
		}
		if val, ok := session.ProcessorState["redirect_url"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Payment.RedirectURL = val
		}
		if val, ok := session.ProcessorState["message"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		} else if val, ok := session.ProcessorState["failure_reason"].(string); ok && strings.TrimSpace(val) != "" {
			resp.Message = strings.TrimSpace(val)
		}
	}

	if action := s.buildNextAction(resp); action != nil {
		resp.NextAction = action
	}

	return resp
}

func normalizeMetadata(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	out := make(map[string]string, len(input))
	for key, value := range input {
		k := strings.TrimSpace(key)
		if k == "" {
			continue
		}
		out[k] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func scopeIdempotencyKey(userID, key string) string {
	trimmedKey := strings.TrimSpace(key)
	if strings.TrimSpace(userID) == "" || trimmedKey == "" {
		return trimmedKey
	}
	return fmt.Sprintf("%s:%s", strings.TrimSpace(userID), trimmedKey)
}

func (s *CheckoutSessionService) isTerminal(status models.CheckoutSessionStatus) bool {
	switch status {
	case models.CheckoutSessionStatusSucceeded,
		models.CheckoutSessionStatusFailed,
		models.CheckoutSessionStatusExpired,
		models.CheckoutSessionStatusCanceled:
		return true
	default:
		return false
	}
}

func (s *CheckoutSessionService) isExpired(session *models.CheckoutSession) bool {
	if session.ExpiresAt == nil || session.ExpiresAt.IsZero() {
		return false
	}
	return session.ExpiresAt.Before(s.now())
}

func (s *CheckoutSessionService) buildNextAction(resp *CheckoutSessionResponse) *CheckoutSessionNextAction {
	if resp == nil {
		return nil
	}
	if resp.Status != string(models.CheckoutSessionStatusRequiresAction) {
		return nil
	}
	if resp.Payment.RedirectURL != "" {
		return &CheckoutSessionNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &CheckoutSessionRedirectToURL{
				URL: resp.Payment.RedirectURL,
			},
		}
	}
	if resp.Payment.TransactionURL != "" {
		return &CheckoutSessionNextAction{
			Type: "solana_qr",
		}
	}
	if resp.Payment.SolanaPayURL != "" {
		return &CheckoutSessionNextAction{
			Type: "solana_pay",
		}
	}
	return nil
}

// getAPIBaseURL returns the API base URL for building Solana Pay URLs.
// Uses config.APIURL which should be set to the full base URL where billing routes are mounted.
//
// Standalone: "https://api.mysite.com" → routes at /v1/*
// Embedded:   "https://api.mysite.com/billing" → routes at /billing/v1/*
//
// Generated URLs follow the pattern: APIURL + {v1Path} + "/checkout/:id/solana-pay"
func (s *CheckoutSessionService) getAPIBaseURL() string {
	if s.config == nil {
		return ""
	}
	apiURL := strings.TrimSpace(s.config.APIURL)
	if apiURL == "" {
		// Fallback for older configs/tests that set Host as a full URL.
		// Only accept values that look like a URL to avoid accidentally emitting
		// unusable links like "0.0.0.0".
		if strings.Contains(s.config.Host, "://") {
			apiURL = strings.TrimSpace(s.config.Host)
		}
	}
	if apiURL == "" {
		return ""
	}
	// Ensure it doesn't end with a slash (we add the version path later)
	return strings.TrimSuffix(apiURL, "/")
}

// getExternalV1Path decides which version path to append to APIURL for externally visible URLs.
//
// Embedded hosts typically mount billing under "/billing", so APIURL ends with "/billing"
// and the external contract becomes "/billing/v1/*".
func (s *CheckoutSessionService) getExternalV1Path(baseURL string) string {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(strings.ToLower(baseURL), "/billing") {
		return "/v1"
	}
	return "/v/1"
}

func (s *CheckoutSessionService) confirmSolanaSession(ctx context.Context, session *models.CheckoutSession, req *CheckoutSessionConfirmRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if strings.TrimSpace(req.Payment.Signature) == "" {
		return nil, fmt.Errorf("%w: signature is required", ErrCheckoutSessionValidation)
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
	}
	if s.checkoutService == nil {
		return nil, fmt.Errorf("%w: checkout service unavailable", ErrCheckoutSessionValidation)
	}
	solanaProc := s.config.GetSolanaProcessor()
	if s.config == nil || solanaProc == nil {
		return nil, fmt.Errorf("%w: solana not configured", ErrCheckoutSessionValidation)
	}

	// Get token symbol from ProcessorState (where initializeSolanaSession stores it)
	tokenSymbol := getStringField(session.ProcessorState, "token_symbol")
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing", ErrCheckoutSessionValidation)
	}

	tokenCfg, ok := solanaProc.SupportedTokens[tokenSymbol]
	if !ok || !tokenCfg.Enabled {
		return nil, fmt.Errorf("%w: unsupported token", ErrCheckoutSessionValidation)
	}
	tokenMint := tokenCfg.Mint
	if strings.EqualFold(solanaProc.Network, "mainnet") && tokenCfg.MainnetMint != "" {
		tokenMint = tokenCfg.MainnetMint
	}
	storedTokenMint := getStringField(session.ProcessorState, "token_mint")
	if storedTokenMint == "" {
		return nil, fmt.Errorf("%w: token_mint missing", ErrCheckoutSessionValidation)
	}
	if !strings.EqualFold(storedTokenMint, tokenMint) {
		return nil, fmt.Errorf("%w: token_mint mismatch", ErrCheckoutSessionValidation)
	}

	expectedAmount := getUint64Field(session.ProcessorState, "token_amount")
	if expectedAmount == 0 {
		return nil, fmt.Errorf("%w: token_amount missing or invalid", ErrCheckoutSessionValidation)
	}
	expectedRecipient := getStringField(session.ProcessorState, "recipient")
	if expectedRecipient == "" {
		return nil, fmt.Errorf("%w: recipient missing", ErrCheckoutSessionValidation)
	}
	// Get payer from ProcessorState (set by BuildSolanaPayTransaction)
	expectedPayer := strings.TrimSpace(getStringField(session.ProcessorState, "payer"))
	if reqWallet := strings.TrimSpace(req.Payment.Wallet); reqWallet != "" {
		if expectedPayer != "" && expectedPayer != reqWallet {
			return nil, fmt.Errorf("%w: wallet does not match session", ErrCheckoutSessionValidation)
		}
		if expectedPayer == "" {
			expectedPayer = reqWallet
		}
	}
	if session.Reference == nil || strings.TrimSpace(*session.Reference) == "" {
		return nil, fmt.Errorf("%w: reference missing", ErrCheckoutSessionValidation)
	}
	referenceValue := strings.TrimSpace(*session.Reference)
	reference := &referenceValue

	if err := s.solanaTransactionService.VerifyTransactionWithContent(
		ctx,
		strings.TrimSpace(req.Payment.Signature),
		expectedAmount,
		expectedRecipient,
		storedTokenMint,
		expectedPayer,
		reference,
	); err != nil {
		return nil, err
	}

	result, err := s.checkoutService.RegisterPurchase(ctx, &RegisterPurchaseRequest{
		UserID:        session.UserID,
		PriceID:       session.PriceID,
		Processor:     "solana",
		TransactionID: strings.TrimSpace(req.Payment.Signature),
		Amount:        session.Amount,
		Currency:      session.Currency,
	})
	if err != nil {
		return nil, err
	}

	if err := s.MarkSucceeded(ctx, session.ID, result.PaymentID, strings.TrimSpace(req.Payment.Signature)); err != nil {
		return nil, err
	}

	updated, err := s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.sessionToResponse(updated), nil
}

func (s *CheckoutSessionService) MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			return nil
		}
		if session.Status != models.CheckoutSessionStatusExpired {
			return ErrCheckoutSessionConflict
		}
	}

	session.Status = models.CheckoutSessionStatusSucceeded
	session.UpdatedAt = s.now()
	if paymentID != uuid.Nil {
		session.PaymentID = &paymentID
	}
	if strings.TrimSpace(transactionID) != "" {
		session.TransactionID = stringPtr(transactionID)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkFailed(ctx context.Context, sessionID uuid.UUID, reason, code string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		switch session.Status {
		case models.CheckoutSessionStatusFailed,
			models.CheckoutSessionStatusSucceeded,
			models.CheckoutSessionStatusExpired,
			models.CheckoutSessionStatusCanceled:
			return nil
		default:
			return ErrCheckoutSessionConflict
		}
	}

	session.Status = models.CheckoutSessionStatusFailed
	session.UpdatedAt = s.now()
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	if msg := strings.TrimSpace(reason); msg != "" {
		session.ProcessorState["message"] = msg
		session.ProcessorState["failure_reason"] = msg
	}
	if strings.TrimSpace(code) != "" {
		session.ProcessorState["failure_code"] = strings.TrimSpace(code)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkExpired(ctx context.Context, sessionID uuid.UUID, message string) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		return nil
	}

	session.Status = models.CheckoutSessionStatusExpired
	session.UpdatedAt = s.now()
	if msg := strings.TrimSpace(message); msg != "" {
		if session.ProcessorState == nil {
			session.ProcessorState = map[string]any{}
		}
		session.ProcessorState["message"] = msg
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) MarkSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID) error {
	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		return ErrCheckoutSessionNotFound
	}
	if s.isTerminal(session.Status) {
		if session.Status == models.CheckoutSessionStatusSucceeded {
			return nil
		}
		if session.Status != models.CheckoutSessionStatusExpired {
			return ErrCheckoutSessionConflict
		}
	}

	session.Status = models.CheckoutSessionStatusSucceeded
	session.UpdatedAt = s.now()
	if paymentID != uuid.Nil {
		session.PaymentID = &paymentID
	}
	if subscriptionID != uuid.Nil {
		session.SubscriptionID = &subscriptionID
	}
	if strings.TrimSpace(transactionID) != "" {
		session.TransactionID = stringPtr(transactionID)
	}

	return s.repo.Update(ctx, session)
}

func (s *CheckoutSessionService) FindOpenByUserPriceProcessor(ctx context.Context, userID string, priceID uuid.UUID, processor models.Processor) (*models.CheckoutSession, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	session, err := s.repo.GetLatestOpenByUserPriceProcessor(ctx, userID, priceID, processor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return session, nil
}

func getStringField(fields map[string]any, key string) string {
	if fields == nil {
		return ""
	}
	raw, ok := fields[key]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	default:
		return ""
	}
}

func getUint64Field(fields map[string]any, key string) uint64 {
	if fields == nil {
		return 0
	}
	raw, ok := fields[key]
	if !ok || raw == nil {
		return 0
	}
	switch val := raw.(type) {
	case uint64:
		return val
	case uint32:
		return uint64(val)
	case uint:
		return uint64(val)
	case int64:
		if val < 0 {
			return 0
		}
		return uint64(val)
	case int:
		if val < 0 {
			return 0
		}
		return uint64(val)
	case float64:
		if val < 0 {
			return 0
		}
		return uint64(val)
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func setSolanaQuoteState(processorState map[string]any, tokenAmount uint64, tokenPriceUSD, fxRate float64, fxCurrency string, quotedAt, quoteExpiresAt time.Time) error {
	if processorState == nil {
		return fmt.Errorf("%w: processor_state unavailable", ErrCheckoutSessionValidation)
	}
	if tokenAmount == 0 {
		return fmt.Errorf("%w: token_amount must be greater than 0", ErrCheckoutSessionValidation)
	}
	if quotedAt.IsZero() {
		return fmt.Errorf("%w: quote timestamp missing", ErrCheckoutSessionValidation)
	}
	if quoteExpiresAt.IsZero() {
		return fmt.Errorf("%w: quote expiry missing", ErrCheckoutSessionValidation)
	}

	processorState["token_amount"] = tokenAmount
	processorState["token_price_usd"] = tokenPriceUSD
	processorState["fx_rate"] = fxRate
	processorState["fx_currency"] = strings.TrimSpace(fxCurrency)
	processorState["quoted_at"] = quotedAt.UTC().Format(time.RFC3339)
	processorState["quote_expires_at"] = quoteExpiresAt.UTC().Format(time.RFC3339)

	return nil
}

func timePtr(t time.Time) *time.Time {
	return &t
}

func stringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	val := strings.TrimSpace(value)
	return &val
}

// SolanaPaySessionInfo contains session info for Solana Pay GET endpoint
type SolanaPaySessionInfo struct {
	ProductName string
}

// SolanaPayTransactionResponse is the response from BuildSolanaPayTransaction
type SolanaPayTransactionResponse struct {
	TransactionBase64 string
	Message           string
}

// GetSessionForSolanaPay retrieves and validates a checkout session for Solana Pay spec endpoints.
// Returns session info needed for GET endpoint or an error if the session is invalid.
func (s *CheckoutSessionService) GetSessionForSolanaPay(ctx context.Context, sessionID uuid.UUID) (*SolanaPaySessionInfo, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	if session == nil {
		return nil, ErrCheckoutSessionNotFound
	}

	// Validate it's a Solana session
	if session.Processor != models.ProcessorSolana {
		return nil, ErrCheckoutSessionNotSolana
	}

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutSessionExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutSessionStatusSucceeded ||
		session.Status == models.CheckoutSessionStatusCanceled {
		return nil, ErrCheckoutSessionAlreadyCompleted
	}

	// Get product name for label (via price)
	var productName string
	if s.priceService != nil {
		price, err := s.priceService.GetByID(ctx, session.PriceID)
		if err == nil && price != nil && s.productService != nil {
			product, err := s.productService.GetByID(ctx, price.ProductID)
			if err == nil && product != nil {
				productName = product.DisplayName
			}
		}
	}

	return &SolanaPaySessionInfo{
		ProductName: productName,
	}, nil
}

// BuildSolanaPayTransaction builds a Solana transaction for the given checkout session and wallet account.
// This implements the POST endpoint of the Solana Pay Transaction Request spec.
func (s *CheckoutSessionService) BuildSolanaPayTransaction(ctx context.Context, sessionID uuid.UUID, account string) (*SolanaPayTransactionResponse, error) {
	if s.repo == nil {
		return nil, ErrCheckoutSessionNotFound
	}
	if s.solanaTransactionService == nil {
		return nil, fmt.Errorf("%w: solana transaction service unavailable", ErrCheckoutSessionValidation)
	}
	solanaProc := s.config.GetSolanaProcessor()
	if solanaProc == nil {
		return nil, fmt.Errorf("%w: solana not configured", ErrCheckoutSessionValidation)
	}

	account = strings.TrimSpace(account)
	if account == "" {
		return nil, fmt.Errorf("%w: account is required", ErrCheckoutSessionValidation)
	}

	session, err := s.repo.GetByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrCheckoutSessionNotFound
		}
		return nil, err
	}
	if session == nil {
		return nil, ErrCheckoutSessionNotFound
	}

	// Validate it's a Solana session
	if session.Processor != models.ProcessorSolana {
		return nil, ErrCheckoutSessionNotSolana
	}

	// Check if expired
	if session.ExpiresAt != nil && s.now().After(*session.ExpiresAt) {
		return nil, ErrCheckoutSessionExpired
	}

	// Check if already completed
	if session.Status == models.CheckoutSessionStatusSucceeded ||
		session.Status == models.CheckoutSessionStatusCanceled {
		return nil, ErrCheckoutSessionAlreadyCompleted
	}

	// Get token symbol from processor state
	tokenSymbol := getStringField(session.ProcessorState, "token_symbol")
	if tokenSymbol == "" {
		return nil, fmt.Errorf("%w: token_symbol missing from session", ErrCheckoutSessionValidation)
	}

	// Generate reference if not already set
	if session.Reference == nil || *session.Reference == "" {
		reference, err := solana.GenerateReference()
		if err != nil {
			return nil, fmt.Errorf("failed to generate reference: %w", err)
		}
		session.Reference = &reference
	}

	// Build the transaction
	txResp, err := s.solanaTransactionService.BuildPaymentTransaction(
		ctx,
		session.UserID,
		session.PriceID,
		tokenSymbol,
		account,
		session.Reference,
	)
	if err != nil {
		return nil, err
	}

	// Update session with payer wallet and transaction info
	if session.ProcessorState == nil {
		session.ProcessorState = map[string]any{}
	}
	session.ProcessorState["payer"] = account
	session.ProcessorState["token_amount"] = txResp.TokenAmount
	session.ProcessorState["recipient"] = solanaProc.RecipientWallet
	session.ExpiresAt = &txResp.ExpiresAt

	if err := s.repo.Update(ctx, session); err != nil {
		return nil, fmt.Errorf("failed to update session: %w", err)
	}

	// Build message for wallet
	message := txResp.Instructions
	if message == "" {
		message = "Sign to complete your payment"
	}

	return &SolanaPayTransactionResponse{
		TransactionBase64: txResp.TransactionBase64,
		Message:           message,
	}, nil
}
