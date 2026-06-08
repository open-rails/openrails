package funding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
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
	httpClient *http.Client
	now        func() time.Time
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

func NewService(repo *repo.USDCFundingSessionRepo, cfg *config.Config) *Service {
	return &Service{
		repo:       repo,
		cfg:        cfg,
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
		case provider == ProviderCoinbase && strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.LaunchURLTemplate) == "":
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
	return session, err
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
	if provider == ProviderCoinbase && strings.TrimSpace(cfg.APIKey) != "" {
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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/platform/v2/onramp/sessions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(cfg.APIKey))
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
