// Package identity names the provider-neutral directory contracts used by
// billing. Hosts explicitly supply their identity integration.
package identity

import "context"

// UserDirectory checks that a user exists and reads their contact identity.
type UserDirectory interface {
	Exists(ctx context.Context, userID string) (bool, error)
	EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error)
}

// UsernameResolver resolves a provider-supplied username to the host's user
// identifier. It is used only by the optional CCBill username bridge.
type UsernameResolver interface {
	GetUserIDByUsername(ctx context.Context, username string) (string, error)
}
