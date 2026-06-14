package repo

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/merchant"
)

var ErrLinkedWalletNotFound = errors.New("linked wallet not found")

type LinkedWalletRepo struct {
	db *db.DB
}

func NewLinkedWalletRepo(d *db.DB) *LinkedWalletRepo { return &LinkedWalletRepo{db: d} }

func linkedWalletFromGen(w gen.OpenrailsLinkedWallet) (*models.LinkedWallet, error) {
	m := &models.LinkedWallet{
		ID:                   w.ID,
		MerchantID:           w.MerchantID,
		CustomerID:           w.CustomerID,
		Chain:                w.Chain,
		Address:              w.Address,
		VerificationProvider: w.VerificationProvider,
		VerifiedAt:           w.VerifiedAt,
		DisplayName:          w.DisplayName,
		CreatedAt:            w.CreatedAt,
		UpdatedAt:            w.UpdatedAt,
	}
	if err := fromJSONB(w.Metadata, &m.Metadata, "linked_wallets.metadata"); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *LinkedWalletRepo) GetByUserIDAndChain(ctx context.Context, userID, chain string) (*models.LinkedWallet, error) {
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	if tsid == uuid.Nil {
		return nil, ErrLinkedWalletNotFound
	}
	row, err := r.db.Gen(ctx).GetLinkedWalletByCustomerAndChain(ctx, gen.GetLinkedWalletByCustomerAndChainParams{
		CustomerID: tsid,
		Chain:      strings.ToLower(strings.TrimSpace(chain)),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrLinkedWalletNotFound
	}
	if err != nil {
		return nil, err
	}
	return linkedWalletFromGen(row)
}

func (r *LinkedWalletRepo) UpsertForUserID(ctx context.Context, userID string, wallet *models.LinkedWallet) (*models.LinkedWallet, error) {
	tsid, err := EnsureCustomerID(ctx, r.db.Qx(ctx), uuid.Nil, userID)
	if err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	wallet.MerchantID = tid.UUID()
	wallet.CustomerID = tsid
	wallet.Chain = strings.ToLower(strings.TrimSpace(wallet.Chain))
	if wallet.Metadata == nil {
		wallet.Metadata = map[string]any{}
	}
	meta, err := toJSONB(wallet.Metadata)
	if err != nil {
		return nil, err
	}
	row, err := r.db.Gen(ctx).UpsertLinkedWallet(ctx, gen.UpsertLinkedWalletParams{
		ID:                   wallet.ID,
		MerchantID:           wallet.MerchantID,
		CustomerID:           wallet.CustomerID,
		Chain:                wallet.Chain,
		Address:              wallet.Address,
		VerificationProvider: wallet.VerificationProvider,
		VerifiedAt:           wallet.VerifiedAt,
		DisplayName:          wallet.DisplayName,
		Metadata:             meta,
		CreatedAt:            wallet.CreatedAt,
		UpdatedAt:            wallet.UpdatedAt,
	})
	if err != nil {
		return nil, err
	}
	updated, err := linkedWalletFromGen(row)
	if err != nil {
		return nil, err
	}
	*wallet = *updated
	return wallet, nil
}

func (r *LinkedWalletRepo) DeleteForUserIDAndChain(ctx context.Context, userID, chain string) error {
	tsid, err := ResolveCustomerID(userID)
	if err != nil {
		return err
	}
	if tsid == uuid.Nil {
		return ErrLinkedWalletNotFound
	}
	rows, err := r.db.Gen(ctx).DeleteLinkedWalletByCustomerAndChain(ctx, gen.DeleteLinkedWalletByCustomerAndChainParams{
		CustomerID: tsid,
		Chain:      strings.ToLower(strings.TrimSpace(chain)),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrLinkedWalletNotFound
	}
	return nil
}
