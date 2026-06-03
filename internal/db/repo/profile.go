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
func (r *ProfileRepo) GetUserEmail(ctx context.Context, id uuid.UUID) (username string, email string, emailVerified bool, isActive bool, err error) {
	// Select minimal fields; tolerate NULL username
	// Using explicit schema-qualified table name.
	type row struct {
		Username      *string `bun:"username"`
		Email         string  `bun:"email"`
		EmailVerified bool    `bun:"email_verified"`
		IsActive      bool    `bun:"is_active"`
	}
	var out row
	q := r.db.Q(ctx).NewSelect().
		TableExpr("profiles.users").
		ColumnExpr("username, email, email_verified, is_active").
		Where("id = ?", id)
	if err = q.Scan(ctx, &out); err != nil {
		return "", "", false, false, err
	}
	if out.Username != nil {
		username = *out.Username
	}
	return username, out.Email, out.EmailVerified, out.IsActive, nil
}

// GetUserIDByUsername looks up a user ID by their username.
// Used by CCBill webhooks to resolve usernames back to user IDs.
func (r *ProfileRepo) GetUserIDByUsername(ctx context.Context, username string) (string, error) {
	var userID string
	q := r.db.Q(ctx).NewSelect().
		TableExpr("profiles.users").
		ColumnExpr("id").
		Where("username = ?", username)
	if err := q.Scan(ctx, &userID); err != nil {
		return "", err
	}
	return userID, nil
}
