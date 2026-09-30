package hostauth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	authkit "github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"
)

type fakeIdentity struct {
	users     map[string]*authkit.AdminUser
	usernames map[string]*authkit.User
	err       error
}

func (f fakeIdentity) AdminGetUser(_ context.Context, id string) (*authkit.AdminUser, error) {
	if u, ok := f.users[id]; ok || f.err != nil {
		return u, f.err
	}
	return nil, authkit.ErrUserNotFound
}

func (f fakeIdentity) GetUserByUsername(_ context.Context, name string) (*authkit.User, error) {
	if u, ok := f.usernames[name]; ok {
		return u, nil
	}
	return nil, authkit.ErrUserNotFound
}

// Billing email goes only to live users with a usable address; lookup misses
// are "no user" but an outage is an error.
func TestDirectoryUsableEmailPolicy(t *testing.T) {
	str := func(s string) *string { return &s }
	gone := time.Now()
	const live, deleted, noEmail, blank = "aaaaaaaa-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"
	d := NewDirectory(fakeIdentity{
		users: map[string]*authkit.AdminUser{
			live:    {ID: live, Email: str("a@example.com"), Username: str("alice")},
			deleted: {ID: deleted, Email: str("d@example.com"), DeletedAt: &gone},
			noEmail: {ID: noEmail},
			blank:   {ID: blank, Email: str("")},
		},
		usernames: map[string]*authkit.User{"alice": {ID: live}, "gone": {ID: deleted, DeletedAt: &gone}},
	})
	ctx := context.Background()

	username, email, ok, err := d.EmailIdentity(ctx, "AAAAAAAA-1111-4111-8111-111111111111")
	require.NoError(t, err)
	require.True(t, ok, "input UUID is canonicalized")
	require.Equal(t, []string{"alice", "a@example.com"}, []string{username, email})
	for _, id := range []string{deleted, noEmail, blank, "55555555-5555-4555-8555-555555555555", "not-a-uuid"} {
		exists, err := d.Exists(ctx, id)
		require.NoError(t, err, id)
		require.False(t, exists, id)
	}
	for _, miss := range []error{pgx.ErrNoRows, authkit.ErrUserNotFound} {
		exists, err := NewDirectory(fakeIdentity{err: miss}).Exists(ctx, live)
		require.NoError(t, err)
		require.False(t, exists)
	}
	outage := errors.New("authkit down")
	_, err = NewDirectory(fakeIdentity{err: outage}).Exists(ctx, live)
	require.ErrorIs(t, err, outage)

	id, err := d.GetUserIDByUsername(ctx, "alice")
	require.NoError(t, err)
	require.Equal(t, live, id)
	for _, name := range []string{"gone", "nobody"} {
		_, err = d.GetUserIDByUsername(ctx, name)
		require.ErrorIs(t, err, authkit.ErrUserNotFound, name)
	}
	require.Nil(t, NewDirectory(nil))
	_, err = (*Directory)(nil).GetUserIDByUsername(ctx, "alice")
	require.ErrorContains(t, err, "client is required")
}
