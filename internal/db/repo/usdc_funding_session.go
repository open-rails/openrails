package repo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

var ErrUSDCFundingSessionNotFound = errors.New("usdc funding session not found")

type USDCFundingSessionRepo struct {
	db *db.DB
}

func NewUSDCFundingSessionRepo(d *db.DB) *USDCFundingSessionRepo {
	return &USDCFundingSessionRepo{db: d}
}

func usdcFundingSessionFromGen(s gen.BillingUsdcFundingSession) (*models.USDCFundingSession, error) {
	m := &models.USDCFundingSession{
		ID:                s.ID,
		TenantID:          s.TenantID,
		TenantSubjectID:   s.TenantSubjectID,
		CheckoutSessionID: s.CheckoutSessionID,
		Provider:          s.Provider,
		WalletAddress:     s.WalletAddress,
		Asset:             s.Asset,
		Network:           s.Network,
		RequestedAmount:   s.RequestedAmount,
		ProviderSessionID: s.ProviderSessionID,
		ProviderURL:       s.ProviderUrl,
		Status:            models.USDCFundingSessionStatus(s.Status),
		ReturnURL:         s.ReturnUrl,
		IdempotencyKey:    s.IdempotencyKey,
		LastCheckedAt:     s.LastCheckedAt,
		ExpiresAt:         s.ExpiresAt,
		CreatedAt:         s.CreatedAt,
		UpdatedAt:         s.UpdatedAt,
	}
	if err := fromJSONB(s.Metadata, &m.Metadata, "usdc_funding_sessions.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *USDCFundingSessionRepo) CreateForUserID(ctx context.Context, userID string, session *models.USDCFundingSession) error {
	tsid, err := EnsureTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	if err := ensureTenantSubjectRow(ctx, r.db.Qx(ctx), uuid.Nil, tsid); err != nil {
		return err
	}
	session.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	session.TenantSubjectID = tsid
	if session.Metadata == nil {
		session.Metadata = map[string]any{}
	}
	meta, err := toJSONB(session.Metadata)
	if err != nil {
		return err
	}
	return r.db.Gen(ctx).CreateUSDCFundingSession(ctx, gen.CreateUSDCFundingSessionParams{
		ID:                session.ID,
		TenantID:          session.TenantID,
		TenantSubjectID:   session.TenantSubjectID,
		CheckoutSessionID: session.CheckoutSessionID,
		Provider:          session.Provider,
		WalletAddress:     session.WalletAddress,
		Asset:             session.Asset,
		Network:           session.Network,
		RequestedAmount:   session.RequestedAmount,
		ProviderSessionID: session.ProviderSessionID,
		ProviderUrl:       session.ProviderURL,
		Status:            string(session.Status),
		ReturnUrl:         session.ReturnURL,
		IdempotencyKey:    session.IdempotencyKey,
		Metadata:          meta,
		LastCheckedAt:     session.LastCheckedAt,
		ExpiresAt:         session.ExpiresAt,
		CreatedAt:         session.CreatedAt,
		UpdatedAt:         session.UpdatedAt,
	})
}

func (r *USDCFundingSessionRepo) GetByIDForUserID(ctx context.Context, id uuid.UUID, userID string) (*models.USDCFundingSession, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrUSDCFundingSessionNotFound
	}
	row, err := r.db.Gen(ctx).GetUSDCFundingSessionByIDForTenantSubject(ctx, gen.GetUSDCFundingSessionByIDForTenantSubjectParams{
		ID:              id,
		TenantSubjectID: tsid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return usdcFundingSessionFromGen(row)
}

func (r *USDCFundingSessionRepo) GetByID(ctx context.Context, id uuid.UUID) (*models.USDCFundingSession, error) {
	row, err := r.db.Gen(ctx).GetUSDCFundingSessionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return usdcFundingSessionFromGen(row)
}

func (r *USDCFundingSessionRepo) GetByIdempotencyKeyForUserID(ctx context.Context, userID, key string) (*models.USDCFundingSession, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrUSDCFundingSessionNotFound
	}
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrUSDCFundingSessionNotFound
	}
	row, err := r.db.Gen(ctx).GetUSDCFundingSessionByIdempotencyKey(ctx, gen.GetUSDCFundingSessionByIdempotencyKeyParams{
		TenantSubjectID: tsid,
		IdempotencyKey:  &key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUSDCFundingSessionNotFound
	}
	if err != nil {
		return nil, err
	}
	return usdcFundingSessionFromGen(row)
}

func (r *USDCFundingSessionRepo) UpdateStatus(ctx context.Context, session *models.USDCFundingSession, status models.USDCFundingSessionStatus, checkedAt *time.Time, metadata map[string]any) error {
	session.Status = status
	session.LastCheckedAt = checkedAt
	if metadata != nil {
		session.Metadata = metadata
	}
	session.UpdatedAt = time.Now().UTC()
	meta, err := toJSONB(session.Metadata)
	if err != nil {
		return err
	}
	return r.db.Gen(ctx).UpdateUSDCFundingSessionStatus(ctx, gen.UpdateUSDCFundingSessionStatusParams{
		ID:            session.ID,
		Status:        string(session.Status),
		LastCheckedAt: session.LastCheckedAt,
		Metadata:      meta,
		UpdatedAt:     session.UpdatedAt,
	})
}
