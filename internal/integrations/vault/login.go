package vault

import (
	"context"
	"errors"
	"fmt"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// ErrLoggingIn is a call that ended before the host's first Vault login.
var ErrLoggingIn = errors.New("awaiting the host's first login")

const loginPoll = 25 * time.Millisecond

// FirstLogin is the host's first login as its client shows it: helpers/vault's
// Session logs in in the background and sets the token on its client in
// place, so until then the client holds none.
type FirstLogin struct {
	client *vaultapi.Client
	until  time.Time
}

// AwaitFirstLogin holds calls made within wait from now until client holds a
// token; later calls go out as they are.
func AwaitFirstLogin(client *vaultapi.Client, wait time.Duration) *FirstLogin {
	if client == nil {
		return nil
	}
	return &FirstLogin{client: client, until: time.Now().Add(wait)}
}

// Wait returns once the client holds a token or the wait is over; ErrLoggingIn
// (as ErrUnavailable) when ctx ends first.
func (l *FirstLogin) Wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	for l.client.Token() == "" {
		left := time.Until(l.until)
		if left <= 0 {
			return nil
		}
		poll := time.NewTimer(min(left, loginPoll))
		select {
		case <-ctx.Done():
			poll.Stop()
			return fmt.Errorf("%w: %w", ErrUnavailable, ErrLoggingIn)
		case <-poll.C:
		}
	}
	return nil
}
