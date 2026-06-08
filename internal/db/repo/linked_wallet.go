package repo

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/tenant"
)

var ErrLinkedWalletNotFound = errors.New("linked wallet not found")

type LinkedWalletRepo struct {
	db *db.DB
}

func NewLinkedWalletRepo(d *db.DB) *LinkedWalletRepo { return &LinkedWalletRepo{db: d} }

func (r *LinkedWalletRepo) GetByUserIDAndChain(ctx context.Context, userID, chain string) (*models.LinkedWallet, error) {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrLinkedWalletNotFound
	}
	wallet := new(models.LinkedWallet)
	err = r.db.Q(ctx).NewSelect().Model(wallet).
		Where("lw.tenant_subject_id = ?", tsid).
		Where("lw.chain = ?", strings.ToLower(strings.TrimSpace(chain))).
		Limit(1).
		Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLinkedWalletNotFound
	}
	return wallet, err
}

func (r *LinkedWalletRepo) UpsertForUserID(ctx context.Context, userID string, wallet *models.LinkedWallet) (*models.LinkedWallet, error) {
	tsid, err := EnsureTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	wallet.TenantID = tenant.FromContextOrDefault(ctx).UUID()
	wallet.TenantSubjectID = tsid
	wallet.Chain = strings.ToLower(strings.TrimSpace(wallet.Chain))
	if wallet.Metadata == nil {
		wallet.Metadata = map[string]any{}
	}
	_, err = r.db.Q(ctx).NewInsert().Model(wallet).
		On(`CONFLICT (tenant_id, tenant_subject_id, chain) DO UPDATE`).
		Set(`address = EXCLUDED.address`).
		Set(`verification_provider = EXCLUDED.verification_provider`).
		Set(`verified_at = EXCLUDED.verified_at`).
		Set(`display_name = EXCLUDED.display_name`).
		Set(`metadata = EXCLUDED.metadata`).
		Set(`updated_at = now()`).
		Returning(`*`).
		Exec(ctx)
	return wallet, err
}

func (r *LinkedWalletRepo) DeleteForUserIDAndChain(ctx context.Context, userID, chain string) error {
	tsid, err := ResolveTenantSubjectID(ctx, r.db.Q(ctx), uuid.Nil, userID)
	if err != nil {
		return err
	}
	if tsid == uuid.Nil {
		return ErrLinkedWalletNotFound
	}
	res, err := r.db.Q(ctx).NewDelete().Model((*models.LinkedWallet)(nil)).
		Where("tenant_subject_id = ?", tsid).
		Where("chain = ?", strings.ToLower(strings.TrimSpace(chain))).
		Exec(ctx)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrLinkedWalletNotFound
	}
	return nil
}
