package openrails

import "context"

// UserDirectory is the small identity-system view OpenRails may use for
// subscription notification email and user-existence checks. OpenRails does
// not own the backing users schema; an embedding host wires this explicitly
// when it wants those integrations.
type UserDirectory interface {
	Exists(ctx context.Context, userID string) (bool, error)
	EmailIdentity(ctx context.Context, userID string) (username, email string, ok bool, err error)
}

// UsernameResolver resolves a provider-supplied username to the host's user
// identifier. It is used only by the optional CCBill username bridge.
type UsernameResolver interface {
	GetUserIDByUsername(ctx context.Context, username string) (string, error)
}
