package service

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// tooBroad is a directory that cannot answer a search in full.
type tooBroad struct{ identity.Directory }

func (tooBroad) SearchCustomers(context.Context, billing.MerchantID, string, int) ([]identity.Contact, error) {
	return nil, identity.ErrSearchTooBroad
}

func TestASearchTooBroadIsRefusedOnSearch(t *testing.T) {
	s := &Service{rt: &app.Runtime{Contacts: tooBroad{}}, Log: slog.New(slog.DiscardHandler)}
	_, err := s.searchCustomers(t.Context(), billing.MerchantID(uuid.New()), "gmail", 10)
	var refused *apperr.Error
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusBadRequest, refused.Status)
	require.Equal(t, billing.CodeInvalidParam, refused.Code)
	require.Equal(t, "search", refused.Param)
}
