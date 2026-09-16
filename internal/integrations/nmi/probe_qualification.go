package nmi

import (
	"context"
	"fmt"
)

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
		return fmt.Errorf("PRODUCTION NMI credentials detected while test_mode is enabled; refusing to arm: use sandbox account credentials or select live mode")
	default:
		return fmt.Errorf("NMI sandbox qualification was indeterminate; refusing to arm")
	}
}
