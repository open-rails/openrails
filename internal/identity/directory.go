// Package identity defines the minimal user-directory contract billing needs
// from the host's identity system, plus a default implementation backed by
// AuthKit's profiles.* tables.
//
// Billing intentionally owns no users table. The only user facts it needs are:
//   - does a user exist (reconcile: never write billing rows for unknown users)
//   - a user's email + username (subscription/payment notification emails)
//
// Standalone and AuthKit-fronted hosts get the profiles-backed implementation
// (ProfilesDirectory) so behavior is identical to before. A non-AuthKit
// embedded host can inject its own UserDirectory, or none at all — in which
// case the dependent features degrade gracefully (see EmailService and the
// reconcile UserExists hook) rather than hard-failing.
package identity

import (
	"context"

	"github.com/google/uuid"
)

// UserDirectory is the host-injectable view of the user store that billing
// depends on. It is deliberately tiny: only the facts billing actually reads.
// All methods are read-only.
type UserDirectory interface {
	// Exists reports whether userID is a known user in the host identity system.
	// Used by reconcile to avoid writing billing rows for users we don't have.
	Exists(ctx context.Context, userID string) (bool, error)

	// EmailIdentity returns the username and email for userID for notification
	// emails. ok is false (with nil error) when the user has no usable email
	// address (missing/inactive/unknown user); callers treat that as "skip the
	// email" rather than an error. A non-nil error indicates a lookup failure.
	EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error)
}

// ProfilesDirectory is the default UserDirectory backed by AuthKit's profiles.*
// tables via the existing ProfileRepo. This is the ONLY place that reads
// profiles.* for the facts above; all billing code depends on the
// UserDirectory interface instead.
type ProfilesDirectory struct {
	profiles *ProfileRepo
}

// NewProfilesDirectory wraps a ProfileRepo as a UserDirectory. Returns nil when
// repo is nil so callers can treat "no directory" uniformly.
func NewProfilesDirectory(profiles *ProfileRepo) *ProfilesDirectory {
	if profiles == nil {
		return nil
	}
	return &ProfilesDirectory{profiles: profiles}
}

// Exists reports whether the user id resolves to a row in profiles.users with a
// usable email (the same liveness signal billing emails rely on). A malformed
// id is treated as "does not exist" rather than an error.
func (d *ProfilesDirectory) Exists(ctx context.Context, userID string) (bool, error) {
	if d == nil || d.profiles == nil {
		return false, nil
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return false, nil
	}
	_, email, _, isActive, err := d.profiles.GetUserEmail(ctx, uid)
	if err != nil {
		return false, err
	}
	return isActive && email != "", nil
}

// EmailIdentity returns the username/email for userID, or ok=false when the user
// has no usable (active, non-empty) email. Mirrors the previous getUserEmail
// behavior exactly.
func (d *ProfilesDirectory) EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error) {
	if d == nil || d.profiles == nil {
		return "", "", false, nil
	}
	uid, perr := uuid.Parse(userID)
	if perr != nil {
		return "", "", false, nil
	}
	uname, mail, _, active, qerr := d.profiles.GetUserEmail(ctx, uid)
	if qerr != nil {
		return "", "", false, qerr
	}
	if mail == "" || !active {
		return "", "", false, nil
	}
	return uname, mail, true, nil
}
