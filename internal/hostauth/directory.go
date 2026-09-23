package hostauth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/google/uuid"
	authkit "github.com/open-rails/authkit"
	"github.com/open-rails/openrails"
)

// IdentityClient is the public AuthKit directory surface this adapter needs.
// authkit.Client satisfies it; no database access is
// borrowed from the billing engine.
type IdentityClient interface {
	AdminGetUser(context.Context, string) (*authkit.AdminUser, error)
	GetUserByUsername(context.Context, string) (*authkit.User, error)
}

// Directory adapts an explicitly supplied AuthKit client for billing email and
// the optional CCBill username bridge.
type Directory struct{ client IdentityClient }

var _ openrails.UserDirectory = (*Directory)(nil)
var _ openrails.UsernameResolver = (*Directory)(nil)

func NewDirectory(client IdentityClient) *Directory {
	if client == nil {
		return nil
	}
	return &Directory{client: client}
}

func (d *Directory) Exists(ctx context.Context, userID string) (bool, error) {
	_, _, ok, err := d.EmailIdentity(ctx, userID)
	return ok, err
}

// EmailIdentity preserves billing's existing usable-email policy.
// Deleted or missing users and empty emails are skipped.
func (d *Directory) EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error) {
	if d == nil || d.client == nil {
		return "", "", false, nil
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return "", "", false, nil
	}
	user, err := d.client.AdminGetUser(ctx, id.String())
	if errors.Is(err, authkit.ErrUserNotFound) || errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if user == nil || user.DeletedAt != nil || user.Email == nil || *user.Email == "" {
		return "", "", false, nil
	}
	if user.Username != nil {
		username = *user.Username
	}
	return username, *user.Email, true, nil
}

// GetUserIDByUsername follows AuthKit's canonical username-resolution policy,
// including retained former names. Billing never reads identity tables.
func (d *Directory) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	if d == nil || d.client == nil {
		return "", fmt.Errorf("authkit directory: client is required")
	}
	user, err := d.client.GetUserByUsername(ctx, username)
	if err != nil {
		return "", err
	}
	if user == nil || user.DeletedAt != nil || user.ID == "" {
		return "", authkit.ErrUserNotFound
	}
	return user.ID, nil
}
