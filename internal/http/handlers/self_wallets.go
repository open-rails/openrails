package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

const solanaWalletChain = "solana"

type linkedWalletResponse struct {
	ID                   string         `json:"id"`
	Object               string         `json:"object"`
	Chain                string         `json:"chain"`
	Address              string         `json:"address"`
	VerificationProvider string         `json:"verification_provider"`
	VerifiedAt           time.Time      `json:"verified_at"`
	DisplayName          *string        `json:"display_name,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

type linkedWalletEnvelope struct {
	Data *linkedWalletResponse `json:"data"`
}

func GetMySolanaWallet(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	wallet, err := repo.NewLinkedWalletRepo(r.State.DB).GetByUserIDAndChain(r.Request.Context(), user.ID, solanaWalletChain)
	if errors.Is(err, repo.ErrLinkedWalletNotFound) {
		r.SuccessJSON(linkedWalletEnvelope{Data: nil})
		return
	}
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to retrieve linked wallet")
		return
	}
	r.SuccessJSON(linkedWalletEnvelope{Data: linkedWalletToResponse(wallet)})
}

func UpsertMySolanaWallet(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	delegated, ok := delegatedFromRequest(r)
	if !ok || strings.TrimSpace(delegated.SolanaAddress) == "" {
		r.ErrorJSON(http.StatusBadRequest, "verified_solana_wallet_required")
		return
	}
	verifiedAt := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(delegated.SolanaVerifiedAt)); err == nil {
		verifiedAt = parsed.UTC()
	}
	var displayName *string
	if name := strings.TrimSpace(delegated.SolanaPrimarySNSName); name != "" {
		displayName = &name
	}
	wallet := &models.LinkedWallet{
		ID:                   uuidutil.NewV7(),
		Chain:                solanaWalletChain,
		Address:              strings.TrimSpace(delegated.SolanaAddress),
		VerificationProvider: "authkit",
		VerifiedAt:           verifiedAt,
		DisplayName:          displayName,
		Metadata: map[string]any{
			"issuer": delegated.Issuer,
		},
	}
	linked, err := repo.NewLinkedWalletRepo(r.State.DB).UpsertForUserID(r.Request.Context(), user.ID, wallet)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to link wallet")
		return
	}
	r.SuccessJSON(linkedWalletEnvelope{Data: linkedWalletToResponse(linked)})
}

func DeleteMySolanaWallet(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}
	err := repo.NewLinkedWalletRepo(r.State.DB).DeleteForUserIDAndChain(r.Request.Context(), user.ID, solanaWalletChain)
	if err != nil && !errors.Is(err, repo.ErrLinkedWalletNotFound) {
		r.ErrorJSON(http.StatusInternalServerError, "failed to unlink wallet")
		return
	}
	r.SuccessJSON(map[string]string{"message": "wallet unlinked"})
}

func delegatedFromRequest(r *httprequest.Request) (*controlplane.ResolvedDelegated, bool) {
	value, ok := r.Get(ginmw.DelegatedContextKey)
	if !ok {
		return nil, false
	}
	delegated, ok := value.(*controlplane.ResolvedDelegated)
	return delegated, ok && delegated != nil
}

func linkedWalletToResponse(wallet *models.LinkedWallet) *linkedWalletResponse {
	if wallet == nil {
		return nil
	}
	return &linkedWalletResponse{
		ID:                   wallet.ID.String(),
		Object:               "linked_wallet",
		Chain:                wallet.Chain,
		Address:              wallet.Address,
		VerificationProvider: wallet.VerificationProvider,
		VerifiedAt:           wallet.VerifiedAt,
		DisplayName:          wallet.DisplayName,
		Metadata:             wallet.Metadata,
		CreatedAt:            wallet.CreatedAt,
		UpdatedAt:            wallet.UpdatedAt,
	}
}
