package repo

import (
	"context"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
)

// ProfileRepo provides read-only access to profiles.users fields we care about.
type ProfileRepo struct{ db *db.DB }

func NewProfileRepo(d *db.DB) *ProfileRepo { return &ProfileRepo{db: d} }

// GetUserEmail fetches username and email for a given user id from profiles.users.
// Returns username (may be empty), email, email_verified, is_active.
//
// is_active is DERIVED as deleted_at IS NULL: the previous bun query selected
// a profiles.users.is_active column that authkit's schema does not define, so
// it failed at runtime with `column "is_active" does not exist` (#334).
func (r *ProfileRepo) GetUserEmail(ctx context.Context, id uuid.UUID) (username string, email string, emailVerified bool, isActive bool, err error) {
	row, err := r.db.Gen(ctx).GetUserEmail(ctx, id)
	if err != nil {
		return "", "", false, false, err
	}
	return row.Username, row.Email, row.EmailVerified, row.IsActive, nil
}

// GetUserIDByUsername looks up a user ID by their username.
// Used by CCBill webhooks to resolve usernames back to user IDs.
func (r *ProfileRepo) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	id, err := r.db.Gen(ctx).GetUserIDByUsername(ctx, &username)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}
