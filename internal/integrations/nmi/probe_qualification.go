package nmi

import (
	"context"
	"errors"
	"fmt"
)

// ErrLiveCredentialsUnderTestMode refuses to arm production credentials in a
// sandbox deployment.
var ErrLiveCredentialsUnderTestMode = errors.New("PRODUCTION NMI credentials detected while test_mode is enabled; refusing to arm: use sandbox account credentials or select live mode")

// CheckTestModeArm requires fresh evidence that an NMI account simulates
// transactions before it may be armed in sandbox mode. Unknown is a refusal;
// neither a missing cache nor an unavailable gateway can authorize live money.
func CheckTestModeArm(ctx context.Context, client *NMIClient) error {
	result, err := client.ProbeTestMode(ctx)
	if err != nil {
		return fmt.Errorf("NMI sandbox qualification failed; refusing to arm: %w", err)
	}
	switch result {
	case ProbeSimulated:
		return nil
	case ProbeLive:
		return ErrLiveCredentialsUnderTestMode
	default:
		return fmt.Errorf("NMI sandbox qualification was indeterminate; refusing to arm")
	}
}
