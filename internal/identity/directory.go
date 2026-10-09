// Package identity is the customer directory billing reads: the facts a host
// pushes with EnsureCustomer (email, username), stored on billing.customers.
// OpenRails never asks the host's auth system about a user.
package identity

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// UserDirectory reads a customer's contact identity.
type UserDirectory interface {
	EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error)
}

// UsernameResolver resolves a provider-supplied username to a customer. It
// is used only by the CCBill username bridge.
type UsernameResolver interface {
	GetUserIDByUsername(ctx context.Context, username string) (string, error)
}

// ErrUsernameUnknown is a username no customer of the merchant declared, or
// one more than one declared.
var ErrUsernameUnknown = errors.New("identity: no customer declared that username")

// Customers answers both from billing.customers, within the merchant ctx is
// pinned to.
type Customers struct{ DB *db.DB }

var (
	_ UserDirectory    = Customers{}
	_ UsernameResolver = Customers{}
)

// EmailIdentity is the customer's declared username and email; ok is false
// without an email.
func (c Customers) EmailIdentity(ctx context.Context, userID string) (string, string, bool, error) {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil || c.DB == nil {
		return "", "", false, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", "", false, err
	}
	row, err := c.DB.Gen(ctx).GetCustomer(ctx, gen.GetCustomerParams{MerchantID: mid.UUID(), ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if row.Email == nil || *row.Email == "" {
		return "", "", false, nil
	}
	username := ""
	if row.Username != nil {
		username = *row.Username
	}
	return username, *row.Email, true, nil
}

// GetUserIDByUsername is the one customer of the merchant that declared
// username (case-insensitively).
func (c Customers) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" || c.DB == nil {
		return "", ErrUsernameUnknown
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	ids, err := c.DB.Gen(ctx).CustomerIDsByUsername(ctx, gen.CustomerIDsByUsernameParams{MerchantID: mid.UUID(), Username: username})
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", ErrUsernameUnknown
	}
	return ids[0].String(), nil
}
