// Package hostauth adapts the control plane's AuthKit client to billing's user
// directory (billing email and the CCBill username bridge).
package hostauth

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/identity"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
)

// IdentityClient is the AuthKit directory surface this adapter needs;
// *authkit.Client satisfies it.
type IdentityClient interface {
	User(context.Context, iam.UserRef, ...authkit.Option) (iam.User, error)
	ResolveUsername(context.Context, string) (iam.NameResolution, error)
}

// Directory adapts an explicitly supplied AuthKit client for billing email and
// the optional CCBill username bridge.
type Directory struct{ client IdentityClient }

var _ identity.UserDirectory = (*Directory)(nil)
var _ identity.UsernameResolver = (*Directory)(nil)

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

// EmailIdentity preserves billing's usable-email policy: deleted or missing
// users and empty emails are skipped.
func (d *Directory) EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error) {
	if d == nil || d.client == nil {
		return "", "", false, nil
	}
	id, err := uuid.Parse(userID)
	if err != nil {
		return "", "", false, nil
	}
	user, err := d.client.User(ctx, iam.UserByID(id.String()))
	if errors.Is(err, iam.ErrUserNotFound) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if user.Email == nil || *user.Email == "" {
		return "", "", false, nil
	}
	return user.Username, *user.Email, true, nil
}

// GetUserIDByUsername follows AuthKit's username resolution, including
// retained former names. Billing never reads identity tables.
func (d *Directory) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	if d == nil || d.client == nil {
		return "", fmt.Errorf("authkit directory: client is required")
	}
	name, err := d.client.ResolveUsername(ctx, username)
	if err != nil {
		return "", err
	}
	return name.ID, nil
}
