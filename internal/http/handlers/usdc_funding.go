package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/funding"
	"github.com/open-rails/openrails/pkg/api"
)

type usdcFundingOptionsQuery struct {
	Wallet            string `form:"wallet"`
	Network           string `form:"network"`
	Asset             string `form:"asset"`
	Amount            string `form:"amount"`
	CheckoutSessionID string `form:"checkout_session_id"`
}

type usdcFundingCreateRequest struct {
	Provider          string `json:"provider" binding:"required"`
	Wallet            string `json:"wallet" binding:"required"`
	Network           string `json:"network" binding:"required"`
	Asset             string `json:"asset,omitempty"`
	Amount            string `json:"amount" binding:"required"`
	CheckoutSessionID string `json:"checkout_session_id,omitempty"`
	ReturnURL         string `json:"return_url,omitempty"`
}

type usdcFundingOptionsResponse struct {
	Data []funding.Option `json:"data"`
}

type usdcFundingSessionResponse struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Provider          string         `json:"provider"`
	Wallet            string         `json:"wallet"`
	Asset             string         `json:"asset"`
	Network           string         `json:"network"`
	Amount            string         `json:"amount"`
	Status            string         `json:"status"`
	ProviderURL       string         `json:"provider_url"`
	ProviderSessionID *string        `json:"provider_session_id,omitempty"`
	CheckoutSessionID *string        `json:"checkout_session_id,omitempty"`
	ReturnURL         *string        `json:"return_url,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	LastCheckedAt     *time.Time     `json:"last_checked_at,omitempty"`
	ExpiresAt         *time.Time     `json:"expires_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

func GetUSDCFundingOptions(r *httprequest.Request) {
	var query usdcFundingOptionsQuery
	if !r.BindQuery(&query) {
		return
	}
	var checkoutID uuid.UUID
	if strings.TrimSpace(query.CheckoutSessionID) != "" {
		parsed, err := api.ParseCheckoutSessionID(query.CheckoutSessionID)
		if err != nil {
			r.APIError(api.InvalidParamError("checkout_session_id", "invalid checkout session id"))
			return
		}
		checkoutID = parsed
	}
	svc := funding.NewService(repo.NewUSDCFundingSessionRepo(r.State.DB), r.State.Config)
	resp := usdcFundingOptionsResponse{Data: svc.Options(funding.OptionsRequest{
		WalletAddress:     query.Wallet,
		Network:           query.Network,
		Asset:             query.Asset,
		Amount:            query.Amount,
		CheckoutSessionID: checkoutID,
	})}
	r.SuccessJSON(resp)
}

func CreateUSDCFundingSession(r *httprequest.Request) {
	var req usdcFundingCreateRequest
	if !r.BindJSON(&req) {
		return
	}
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	var checkoutID uuid.UUID
	if strings.TrimSpace(req.CheckoutSessionID) != "" {
		parsed, err := api.ParseCheckoutSessionID(req.CheckoutSessionID)
		if err != nil {
			r.APIError(api.InvalidParamError("checkout_session_id", "invalid checkout session id"))
			return
		}
		checkoutID = parsed
	}
	svc := funding.NewService(repo.NewUSDCFundingSessionRepo(r.State.DB), r.State.Config)
	session, err := svc.Create(r.Request.Context(), funding.CreateRequest{
		UserID:            user.ID,
		Provider:          req.Provider,
		WalletAddress:     req.Wallet,
		Network:           req.Network,
		Asset:             req.Asset,
		Amount:            req.Amount,
		CheckoutSessionID: checkoutID,
		ReturnURL:         req.ReturnURL,
		IdempotencyKey:    r.Header("X-Idempotency-Key"),
	})
	if err != nil {
		writeUSDCFundingError(r, err)
		return
	}
	r.SuccessJSON(usdcFundingSessionToResponse(session))
}

func GetUSDCFundingSession(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	id, err := api.ParseUSDCFundingSessionID(strings.TrimSpace(r.Param("id")))
	if err != nil {
		r.APIError(api.InvalidIDError("usdc_funding_session"))
		return
	}
	svc := funding.NewService(repo.NewUSDCFundingSessionRepo(r.State.DB), r.State.Config)
	session, err := svc.Get(r.Request.Context(), user.ID, id)
	if err != nil {
		writeUSDCFundingError(r, err)
		return
	}
	r.SuccessJSON(usdcFundingSessionToResponse(session))
}

func writeUSDCFundingError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, funding.ErrProviderUnavailable):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, api.CodeInvalidParam, "USDC funding provider is unavailable for this wallet/network"))
	case errors.Is(err, funding.ErrInvalidRequest):
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, api.CodeInvalidParam, err.Error()))
	case errors.Is(err, funding.ErrSessionNotFound):
		r.APIError(api.NotFoundError("usdc_funding_session"))
	default:
		r.ErrorJSON(http.StatusInternalServerError, "USDC funding request failed")
	}
}

func usdcFundingSessionToResponse(session *models.USDCFundingSession) usdcFundingSessionResponse {
	var checkoutID *string
	if session.CheckoutSessionID != nil {
		value := api.FormatCheckoutSessionID(*session.CheckoutSessionID)
		checkoutID = &value
	}
	return usdcFundingSessionResponse{
		ID:                api.FormatUSDCFundingSessionID(session.ID),
		Object:            "usdc_funding_session",
		Provider:          session.Provider,
		Wallet:            session.WalletAddress,
		Asset:             session.Asset,
		Network:           session.Network,
		Amount:            session.RequestedAmount,
		Status:            string(session.Status),
		ProviderURL:       session.ProviderURL,
		ProviderSessionID: session.ProviderSessionID,
		CheckoutSessionID: checkoutID,
		ReturnURL:         session.ReturnURL,
		Metadata:          session.Metadata,
		LastCheckedAt:     session.LastCheckedAt,
		ExpiresAt:         session.ExpiresAt,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         session.UpdatedAt,
	}
}
