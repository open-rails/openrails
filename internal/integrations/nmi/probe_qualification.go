package nmi

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/internal/providerposture"
)

// ErrLiveCredentialsUnderTestMode refuses to arm production credentials in a
// sandbox deployment.
var ErrLiveCredentialsUnderTestMode = errors.New("PRODUCTION NMI credentials detected while test_mode is enabled; refusing to arm: use sandbox account credentials or select live mode")

// CheckTestModeArm verifies the credential now (recording the verdict for
// every client that loads it) and refuses anything but a simulated account.
func CheckTestModeArm(ctx context.Context, client *NMIClient) error {
	status := providerposture.Process().Verify(ctx, client.PostureKey(), client.CheckPosture)
	switch status.Verdict {
	case providerposture.Simulated:
		return nil
	case providerposture.Live:
		return ErrLiveCredentialsUnderTestMode
	default:
		return fmt.Errorf("NMI sandbox qualification failed; refusing to arm: %w", status.Err)
	}
}
