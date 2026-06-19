package funding

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
)

const (
	ProviderRobinhood = "robinhood"
	ProviderCoinbase  = "coinbase"
)

var (
	ErrInvalidRequest      = errors.New("funding: invalid request")
	ErrProviderUnavailable = errors.New("funding: provider unavailable")
	ErrSessionNotFound     = errors.New("funding: session not found")
)

type Service struct {
	repo       *repo.USDCFundingSessionRepo
	cfg        *config.Config
	processors config.ProcessorSet
	httpClient *http.Client
	balances   SolanaBalanceReader
	now        func() time.Time
}

type SolanaBalanceReader interface {
	GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error)
}

type Option struct {
	Provider string   `json:"provider"`
	Label    string   `json:"label"`
	Rank     int      `json:"rank"`
	Asset    string   `json:"asset"`
	Networks []string `json:"networks"`
	Network  string   `json:"network"`
	Enabled  bool     `json:"enabled"`
	Reason   string   `json:"reason,omitempty"`
}

type OptionsRequest struct {
	WalletAddress     string
	Network           string
	Asset             string
	Amount            string
	CheckoutSessionID uuid.UUID
}

type CreateRequest struct {
	UserID            string
	Provider          string
	WalletAddress     string
	Network           string
	Asset             string
	Amount            string
	CheckoutSessionID uuid.UUID
	ReturnURL         string
	IdempotencyKey    string
}

type ProviderWebhookEvent struct {
	Provider  string
	SessionID uuid.UUID
	EventType string
	Status    string
	Payload   map[string]any
}

func NewService(repo *repo.USDCFundingSessionRepo, cfg *config.Config, processorSets ...config.ProcessorSet) *Service {
	var processors config.ProcessorSet
	if len(processorSets) > 0 {
		processors = processorSets[0]
	}
	return &Service{
		repo:       repo,
		cfg:        cfg,
		processors: processors,
		httpClient: http.DefaultClient,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) WithHTTPClient(c *http.Client) *Service {
	if c != nil {
		s.httpClient = c
	}
	return s
}

func (s *Service) WithSolanaBalanceReader(reader SolanaBalanceReader) *Service {
	if reader != nil {
		s.balances = reader
	}
	return s
}

func (s *Service) Options(req OptionsRequest) []Option {
	network := normalizeNetwork(req.Network)
	asset := normalizeAsset(req.Asset)
	providers := []string{ProviderRobinhood, ProviderCoinbase}
	out := make([]Option, 0, len(providers))
	for i, provider := range providers {
		cfg := s.providerConfig(provider)
		networks := supportedNetworks(provider, cfg)
		opt := Option{
			Provider: provider,
			Label:    providerLabel(provider),
			Rank:     i + 1,
			Asset:    "USDC",
			Networks: networks,
			Network:  network,
			Enabled:  true,
		}
		switch {
		case cfg == nil || !cfg.Enabled:
			opt.Enabled = false
			opt.Reason = "provider_not_configured"
		case asset != "USDC":
			opt.Enabled = false
			opt.Reason = "asset_not_supported"
		case network == "":
			opt.Enabled = false
			opt.Reason = "network_required"
		case !networkSupported(network, networks):
			opt.Enabled = false
			opt.Reason = "network_not_supported"
		case !validSolanaWallet(req.WalletAddress):
			opt.Enabled = false
			opt.Reason = "wallet_network_mismatch"
		case provider == ProviderRobinhood && strings.TrimSpace(cfg.LaunchURLTemplate) == "":
			opt.Enabled = false
			opt.Reason = "partner_handoff_not_configured"
		case provider == ProviderCoinbase && !coinbaseAPIConfigured(cfg) && strings.TrimSpace(cfg.LaunchURLTemplate) == "":
			opt.Enabled = false
			opt.Reason = "coinbase_api_not_configured"
		}
		out = append(out, opt)
	}
	return out
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (*models.USDCFundingSession, error) {
	if s.repo == nil {
		return nil, fmt.Errorf("funding: repository unavailable")
	}
	userID := strings.TrimSpace(req.UserID)
	if userID == "" {
		return nil, fmt.Errorf("%w: user_id required", ErrInvalidRequest)
	}
	if existing, err := s.repo.GetByIdempotencyKeyForUserID(ctx, userID, req.IdempotencyKey); err == nil {
		return existing, nil
	} else if !errors.Is(err, repo.ErrUSDCFundingSessionNotFound) {
		return nil, err
	}

	provider := normalizeProvider(req.Provider)
	network := normalizeNetwork(req.Network)
	asset := normalizeAsset(req.Asset)
	amount := strings.TrimSpace(req.Amount)
	if provider == "" {
		return nil, fmt.Errorf("%w: provider required", ErrInvalidRequest)
	}
	if asset != "USDC" {
		return nil, fmt.Errorf("%w: asset must be USDC", ErrInvalidRequest)
	}
	if amount == "" {
		return nil, fmt.Errorf("%w: amount required", ErrInvalidRequest)
	}
	if !validSolanaWallet(req.WalletAddress) {
		return nil, fmt.Errorf("%w: wallet does not match network", ErrInvalidRequest)
	}
	if !providerEligible(s.Options(OptionsRequest{WalletAddress: req.WalletAddress, Network: network, Asset: asset, Amount: amount}), provider) {
		return nil, ErrProviderUnavailable
	}

	id := uuidutil.NewV7()
	expiresAt := s.now().Add(30 * time.Minute)
	sessionID := api.FormatUSDCFundingSessionID(id)
	providerURL, providerSessionID, err := s.createProviderURL(ctx, provider, providerURLRequest{
		SessionID:      sessionID,
		WalletAddress:  strings.TrimSpace(req.WalletAddress),
		Network:        network,
		Asset:          asset,
		Amount:         amount,
		ReturnURL:      strings.TrimSpace(req.ReturnURL),
		PartnerUserRef: sessionID,
	})
	if err != nil {
		return nil, err
	}

	var checkoutID *uuid.UUID
	if req.CheckoutSessionID != uuid.Nil {
		checkoutID = &req.CheckoutSessionID
	}
	var returnURL *string
	if strings.TrimSpace(req.ReturnURL) != "" {
		value := strings.TrimSpace(req.ReturnURL)
		returnURL = &value
	}
	var idem *string
	if strings.TrimSpace(req.IdempotencyKey) != "" {
		value := strings.TrimSpace(req.IdempotencyKey)
		idem = &value
	}
	var providerID *string
	if strings.TrimSpace(providerSessionID) != "" {
		value := strings.TrimSpace(providerSessionID)
		providerID = &value
	}

	session := &models.USDCFundingSession{
		ID:                id,
		CheckoutSessionID: checkoutID,
		Provider:          provider,
		WalletAddress:     strings.TrimSpace(req.WalletAddress),
		Asset:             asset,
		Network:           network,
		RequestedAmount:   amount,
		ProviderSessionID: providerID,
		ProviderURL:       providerURL,
		Status:            models.USDCFundingSessionCreated,
		ReturnURL:         returnURL,
		IdempotencyKey:    idem,
		ExpiresAt:         &expiresAt,
		Metadata: map[string]any{
			"funding_only": true,
		},
		CreatedAt: s.now(),
		UpdatedAt: s.now(),
	}
	if err := s.repo.CreateForUserID(ctx, userID, session); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Service) Get(ctx context.Context, userID string, id uuid.UUID) (*models.USDCFundingSession, error) {
	if s.repo == nil {
		return nil, fmt.Errorf("funding: repository unavailable")
	}
	session, err := s.repo.GetByIDForUserID(ctx, id, userID)
	if errors.Is(err, repo.ErrUSDCFundingSessionNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.refreshSolanaFundingStatus(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Service) ApplyProviderWebhook(ctx context.Context, event ProviderWebhookEvent) (*models.USDCFundingSession, error) {
	if s.repo == nil {
		return nil, fmt.Errorf("funding: repository unavailable")
	}
	provider := normalizeProvider(event.Provider)
	if provider == "" {
		return nil, fmt.Errorf("%w: provider required", ErrInvalidRequest)
	}
	if event.SessionID == uuid.Nil {
		return nil, fmt.Errorf("%w: session_id required", ErrInvalidRequest)
	}
	session, err := s.repo.GetByID(ctx, event.SessionID)
	if errors.Is(err, repo.ErrUSDCFundingSessionNotFound) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	if normalizeProvider(session.Provider) != provider {
		return nil, ErrSessionNotFound
	}
	if !fundingStatusTerminal(session.Status) {
		status := providerWebhookStatus(event.EventType, event.Status)
		if status != "" {
			if err := s.updateSessionStatus(ctx, session, status, nil, map[string]any{
				"provider_webhook": map[string]any{
					"provider":    provider,
					"event_type":  strings.TrimSpace(event.EventType),
					"status":      strings.TrimSpace(event.Status),
					"payload":     event.Payload,
					"received_at": s.now().Format(time.RFC3339Nano),
				},
			}); err != nil {
				return nil, err
			}
		}
	}
	if err := s.refreshSolanaFundingStatus(ctx, session); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Service) refreshSolanaFundingStatus(ctx context.Context, session *models.USDCFundingSession) error {
	if session == nil || session.Network != "solana" || session.Asset != "USDC" {
		return nil
	}
	switch session.Status {
	case models.USDCFundingSessionFunded, models.USDCFundingSessionFailed, models.USDCFundingSessionCancelled:
		return nil
	}

	checkedAt := s.now()
	if s.balances == nil {
		if session.Status == models.USDCFundingSessionCreated || session.Status == models.USDCFundingSessionOpened {
			return s.updateSessionStatus(ctx, session, models.USDCFundingSessionPendingSettlement, &checkedAt, map[string]any{
				"balance_check": map[string]any{
					"checked_at": checkedAt.Format(time.RFC3339Nano),
					"network":    "solana",
					"asset":      "USDC",
					"reason":     "solana_rpc_unavailable",
				},
			})
		}
		return nil
	}

	mint, decimals, err := s.usdcMintConfig()
	if err != nil {
		return s.updateSessionStatus(ctx, session, models.USDCFundingSessionPendingSettlement, &checkedAt, map[string]any{
			"balance_check": map[string]any{
				"checked_at": checkedAt.Format(time.RFC3339Nano),
				"network":    "solana",
				"asset":      "USDC",
				"reason":     err.Error(),
			},
		})
	}
	required, err := decimalAmountToBaseUnits(session.RequestedAmount, decimals)
	if err != nil {
		return s.updateSessionStatus(ctx, session, models.USDCFundingSessionPendingSettlement, &checkedAt, map[string]any{
			"balance_check": map[string]any{
				"checked_at": checkedAt.Format(time.RFC3339Nano),
				"network":    "solana",
				"asset":      "USDC",
				"mint":       mint.String(),
				"reason":     err.Error(),
			},
		})
	}

	wallet, err := solanago.PublicKeyFromBase58(strings.TrimSpace(session.WalletAddress))
	if err != nil {
		return s.updateSessionStatus(ctx, session, models.USDCFundingSessionPendingSettlement, &checkedAt, map[string]any{
			"balance_check": map[string]any{
				"checked_at": checkedAt.Format(time.RFC3339Nano),
				"network":    "solana",
				"asset":      "USDC",
				"mint":       mint.String(),
				"reason":     "invalid_wallet",
			},
		})
	}
	actual, err := s.balances.GetTokenBalanceForMint(ctx, wallet, mint)
	if err != nil {
		return s.updateSessionStatus(ctx, session, models.USDCFundingSessionPendingSettlement, &checkedAt, map[string]any{
			"balance_check": map[string]any{
				"checked_at":           checkedAt.Format(time.RFC3339Nano),
				"network":              "solana",
				"asset":                "USDC",
				"mint":                 mint.String(),
				"required_base_units":  strconv.FormatUint(required, 10),
				"provider_session_id":  stringPtrValue(session.ProviderSessionID),
				"reason":               "balance_read_failed",
				"balance_read_message": err.Error(),
			},
		})
	}

	nextStatus := models.USDCFundingSessionPendingSettlement
	if session.ExpiresAt != nil && checkedAt.After(*session.ExpiresAt) {
		nextStatus = models.USDCFundingSessionExpired
	}
	if actual >= required {
		nextStatus = models.USDCFundingSessionFunded
	}
	return s.updateSessionStatus(ctx, session, nextStatus, &checkedAt, map[string]any{
		"balance_check": map[string]any{
			"checked_at":          checkedAt.Format(time.RFC3339Nano),
			"network":             "solana",
			"asset":               "USDC",
			"mint":                mint.String(),
			"required_base_units": strconv.FormatUint(required, 10),
			"actual_base_units":   strconv.FormatUint(actual, 10),
			"funded":              actual >= required,
		},
	})
}

func fundingStatusTerminal(status models.USDCFundingSessionStatus) bool {
	switch status {
	case models.USDCFundingSessionFunded, models.USDCFundingSessionFailed, models.USDCFundingSessionCancelled:
		return true
	default:
		return false
	}
}

func providerWebhookStatus(eventType, providerStatus string) models.USDCFundingSessionStatus {
	status := strings.ToLower(strings.TrimSpace(providerStatus))
	event := strings.ToLower(strings.TrimSpace(eventType))
	switch {
	case strings.Contains(event, ".success"),
		strings.Contains(status, "completed"),
		strings.Contains(status, "complete"),
		strings.Contains(status, "success"),
		strings.Contains(status, "settled"),
		strings.Contains(status, "fulfilled"):
		return models.USDCFundingSessionPendingSettlement
	case strings.Contains(event, ".failed"),
		strings.Contains(status, "failed"),
		strings.Contains(status, "failure"),
		strings.Contains(status, "rejected"),
		strings.Contains(status, "error"):
		return models.USDCFundingSessionFailed
	case strings.Contains(status, "cancelled"), strings.Contains(status, "canceled"):
		return models.USDCFundingSessionCancelled
	case strings.Contains(status, "expired"):
		return models.USDCFundingSessionExpired
	case strings.Contains(event, ".created"), strings.Contains(event, ".updated"),
		strings.Contains(status, "created"),
		strings.Contains(status, "pending"),
		strings.Contains(status, "in_progress"),
		strings.Contains(status, "processing"):
		return models.USDCFundingSessionPendingProvider
	default:
		return ""
	}
}

func (s *Service) updateSessionStatus(ctx context.Context, session *models.USDCFundingSession, status models.USDCFundingSessionStatus, checkedAt *time.Time, metadata map[string]any) error {
	if session.Status == status && metadata == nil {
		return nil
	}
	merged := mergeMetadata(session.Metadata, metadata)
	if s.repo == nil {
		session.Status = status
		session.LastCheckedAt = checkedAt
		session.Metadata = merged
		session.UpdatedAt = s.now()
		return nil
	}
	return s.repo.UpdateStatus(ctx, session, status, checkedAt, merged)
}

func (s *Service) usdcMintConfig() (solanago.PublicKey, int, error) {
	proc := s.processors.GetSolanaProcessor()
	if proc == nil || proc.Tokens == nil {
		return solanago.PublicKey{}, 0, fmt.Errorf("solana_usdc_not_configured")
	}
	token, ok := proc.Tokens["USDC"]
	if !ok || strings.TrimSpace(token.Mint) == "" {
		return solanago.PublicKey{}, 0, fmt.Errorf("solana_usdc_not_configured")
	}
	if token.Decimals < 0 || token.Decimals > 9 {
		return solanago.PublicKey{}, 0, fmt.Errorf("solana_usdc_decimals_invalid")
	}
	mint, err := solanago.PublicKeyFromBase58(token.Mint)
	if err != nil {
		return solanago.PublicKey{}, 0, fmt.Errorf("solana_usdc_mint_invalid")
	}
	return mint, token.Decimals, nil
}

type providerURLRequest struct {
	SessionID      string
	WalletAddress  string
	Network        string
	Asset          string
	Amount         string
	ReturnURL      string
	PartnerUserRef string
}

func (s *Service) createProviderURL(ctx context.Context, provider string, req providerURLRequest) (string, string, error) {
	cfg := s.providerConfig(provider)
	if cfg == nil {
		return "", "", ErrProviderUnavailable
	}
	if provider == ProviderCoinbase && coinbaseAPIConfigured(cfg) {
		return s.createCoinbaseURL(ctx, cfg, req)
	}
	if tmpl := strings.TrimSpace(cfg.LaunchURLTemplate); tmpl != "" {
		return expandURLTemplate(tmpl, req), req.SessionID, nil
	}
	return "", "", ErrProviderUnavailable
}

func (s *Service) createCoinbaseURL(ctx context.Context, cfg *config.USDCFundingProviderConfig, req providerURLRequest) (string, string, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if base == "" {
		base = "https://api.cdp.coinbase.com"
	}
	body := map[string]string{
		"purchaseCurrency":   req.Asset,
		"destinationNetwork": req.Network,
		"destinationAddress": req.WalletAddress,
		"purchaseAmount":     req.Amount,
		"paymentCurrency":    "USD",
		"partnerUserRef":     req.PartnerUserRef,
	}
	if req.ReturnURL != "" {
		body["redirectUrl"] = req.ReturnURL
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	endpoint, err := url.Parse(base + "/platform/v2/onramp/sessions")
	if err != nil {
		return "", "", fmt.Errorf("coinbase create onramp session endpoint: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	bearer, err := coinbaseBearerToken(cfg, http.MethodPost, endpoint.Host, endpoint.RequestURI(), s.now())
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("coinbase create onramp session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("coinbase create onramp session failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		Session struct {
			OnrampURL string `json:"onrampUrl"`
		} `json:"session"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", "", fmt.Errorf("coinbase create onramp session decode: %w", err)
	}
	if strings.TrimSpace(out.Session.OnrampURL) == "" {
		return "", "", fmt.Errorf("coinbase create onramp session: missing onramp url")
	}
	return strings.TrimSpace(out.Session.OnrampURL), req.SessionID, nil
}

func coinbaseAPIConfigured(cfg *config.USDCFundingProviderConfig) bool {
	if cfg == nil {
		return false
	}
	if strings.TrimSpace(cfg.APIKey) != "" {
		return true
	}
	return strings.TrimSpace(cfg.APIKeyID) != "" && strings.TrimSpace(cfg.APIKeySecret) != ""
}

func coinbaseBearerToken(cfg *config.USDCFundingProviderConfig, method, host, path string, now time.Time) (string, error) {
	if strings.TrimSpace(cfg.APIKeyID) == "" || strings.TrimSpace(cfg.APIKeySecret) == "" {
		token := strings.TrimSpace(cfg.APIKey)
		if token == "" {
			return "", fmt.Errorf("coinbase bearer token: api key id/secret required")
		}
		return token, nil
	}
	key, signingMethod, err := parseCoinbaseSigningKey(cfg.APIKeySecret)
	if err != nil {
		return "", fmt.Errorf("coinbase bearer token: %w", err)
	}
	claims := jwt.MapClaims{
		"iss": "cdp",
		"sub": strings.TrimSpace(cfg.APIKeyID),
		"aud": []string{"cdp_service"},
		"nbf": now.Unix(),
		"exp": now.Add(2 * time.Minute).Unix(),
		"uri": fmt.Sprintf("%s %s%s", strings.ToUpper(method), host, path),
	}
	token := jwt.NewWithClaims(signingMethod, claims)
	token.Header["kid"] = strings.TrimSpace(cfg.APIKeyID)
	token.Header["nonce"] = strings.ReplaceAll(uuid.NewString(), "-", "")
	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signed, nil
}

func parseCoinbaseSigningKey(secret string) (any, jwt.SigningMethod, error) {
	secret = strings.TrimSpace(strings.ReplaceAll(secret, `\n`, "\n"))
	if secret == "" {
		return nil, nil, fmt.Errorf("api key secret required")
	}
	if block, _ := pem.Decode([]byte(secret)); block != nil {
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			if parsed, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes); parseErr == nil {
				if ec, ok := parsed.(*ecdsa.PrivateKey); ok {
					return ec, jwt.SigningMethodES256, nil
				}
			}
			return nil, nil, fmt.Errorf("parse ECDSA private key: %w", err)
		}
		return key, jwt.SigningMethodES256, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return nil, nil, fmt.Errorf("decode Ed25519 key: %w", err)
	}
	switch len(decoded) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(decoded), jwt.SigningMethodEdDSA, nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(decoded), jwt.SigningMethodEdDSA, nil
	default:
		return nil, nil, fmt.Errorf("invalid Ed25519 key length %d", len(decoded))
	}
}

func (s *Service) providerConfig(provider string) *config.USDCFundingProviderConfig {
	if s == nil || s.cfg == nil || s.cfg.USDCFunding == nil || s.cfg.USDCFunding.Providers == nil {
		return nil
	}
	return s.cfg.USDCFunding.Providers[provider]
}

func providerEligible(opts []Option, provider string) bool {
	for _, opt := range opts {
		if opt.Provider == provider {
			return opt.Enabled
		}
	}
	return false
}

func supportedNetworks(provider string, cfg *config.USDCFundingProviderConfig) []string {
	if cfg != nil && len(cfg.SupportedNetworks) > 0 {
		out := make([]string, 0, len(cfg.SupportedNetworks))
		for _, n := range cfg.SupportedNetworks {
			if normalized := normalizeNetwork(n); normalized != "" {
				out = append(out, normalized)
			}
		}
		return out
	}
	switch provider {
	case ProviderRobinhood, ProviderCoinbase:
		return []string{"solana"}
	default:
		return nil
	}
}

func networkSupported(network string, networks []string) bool {
	for _, candidate := range networks {
		if normalizeNetwork(candidate) == network {
			return true
		}
	}
	return false
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderRobinhood:
		return ProviderRobinhood
	case ProviderCoinbase:
		return ProviderCoinbase
	default:
		return ""
	}
}

func normalizeAsset(asset string) string {
	if strings.TrimSpace(asset) == "" {
		return "USDC"
	}
	return strings.ToUpper(strings.TrimSpace(asset))
}

func normalizeNetwork(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case "solana", "solana-mainnet", "mainnet-beta":
		return "solana"
	default:
		return ""
	}
}

func validSolanaWallet(wallet string) bool {
	wallet = strings.TrimSpace(wallet)
	if wallet == "" {
		return false
	}
	_, err := solanago.PublicKeyFromBase58(wallet)
	return err == nil
}

func providerLabel(provider string) string {
	switch provider {
	case ProviderRobinhood:
		return "Robinhood"
	case ProviderCoinbase:
		return "Coinbase"
	default:
		return provider
	}
}

func expandURLTemplate(tmpl string, req providerURLRequest) string {
	replacer := strings.NewReplacer(
		"{wallet}", url.QueryEscape(req.WalletAddress),
		"{network}", url.QueryEscape(req.Network),
		"{asset}", url.QueryEscape(req.Asset),
		"{amount}", url.QueryEscape(req.Amount),
		"{session_id}", url.QueryEscape(req.SessionID),
		"{return_url}", url.QueryEscape(req.ReturnURL),
	)
	return replacer.Replace(tmpl)
}

func mergeMetadata(existing, update map[string]any) map[string]any {
	out := make(map[string]any, len(existing)+len(update))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range update {
		out[k] = v
	}
	return out
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func decimalAmountToBaseUnits(amount string, decimals int) (uint64, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" {
		return 0, fmt.Errorf("amount_required")
	}
	if strings.HasPrefix(amount, "-") {
		return 0, fmt.Errorf("amount_must_be_positive")
	}
	whole, frac, hasFrac := strings.Cut(amount, ".")
	if whole == "" {
		whole = "0"
	}
	if whole == "" || !allDigits(whole) {
		return 0, fmt.Errorf("amount_invalid")
	}
	if hasFrac && !allDigits(frac) {
		return 0, fmt.Errorf("amount_invalid")
	}
	if len(frac) > decimals {
		return 0, fmt.Errorf("amount_precision_exceeds_token_decimals")
	}
	for len(frac) < decimals {
		frac += "0"
	}

	scale, ok := pow10Uint64(decimals)
	if !ok {
		return 0, fmt.Errorf("amount_decimals_invalid")
	}
	wholeUnits, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount_invalid")
	}
	fracUnits := uint64(0)
	if frac != "" {
		fracUnits, err = strconv.ParseUint(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("amount_invalid")
		}
	}
	if wholeUnits > (^uint64(0)-fracUnits)/scale {
		return 0, fmt.Errorf("amount_overflow")
	}
	baseUnits := wholeUnits*scale + fracUnits
	if baseUnits == 0 {
		return 0, fmt.Errorf("amount_must_be_positive")
	}
	return baseUnits, nil
}

func allDigits(value string) bool {
	if value == "" {
		return true
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func pow10Uint64(decimals int) (uint64, bool) {
	if decimals < 0 {
		return 0, false
	}
	out := uint64(1)
	for i := 0; i < decimals; i++ {
		if out > ^uint64(0)/10 {
			return 0, false
		}
		out *= 10
	}
	return out, true
}
