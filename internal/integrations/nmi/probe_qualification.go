package nmi

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/providerposture"
)

// ErrEnvironmentMismatch refuses to arm an NMI account whose own answer
// disagrees with its PSP's declared environment: a live PSP whose account is
// in test mode or on the sandbox, or a test or sandbox PSP that moves real
// money.
var ErrEnvironmentMismatch = errors.New("the NMI account is not in the PSP's declared environment; refusing to arm")

// CheckEnvironmentArm verifies the credential now (recording the verdict for
// every client that loads it) and refuses an account NMI does not prove to be
// in the declared environment.
func CheckEnvironmentArm(ctx context.Context, client *NMIClient) error {
	status := providerposture.Process().Verify(ctx, client.PostureKey(), client.PostureCheck())
	if status.Armed() {
		return nil
	}
	if status.Verdict == providerposture.Live || status.Verdict == providerposture.Simulated {
		return status.Err
	}
	return fmt.Errorf("NMI %s qualification failed; refusing to arm: %w", client.Environment, status.Err)
}
