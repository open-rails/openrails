package solana

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyTransferRequiresExpectedContentFields(t *testing.T) {
	t.Parallel()

	client := &RPCClient{}

	t.Run("requires amount", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    0,
			ExpectedRecipient: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			ExpectedReference: "11111111111111111111111111111112",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected amount must be greater than 0")
	})

	t.Run("requires recipient", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    123,
			ExpectedRecipient: "",
			ExpectedReference: "11111111111111111111111111111112",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected recipient is required")
	})

	t.Run("requires reference", func(t *testing.T) {
		t.Parallel()

		err := client.VerifyTransfer(context.Background(), VerifyTransferRequest{
			Signature:         "dummy-signature",
			ExpectedAmount:    123,
			ExpectedRecipient: "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh",
			ExpectedReference: "",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected reference is required")
	})
}
