package solana

import (
	"context"
	"testing"

	"github.com/google/uuid"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/stretchr/testify/require"
)

// TestTransferRequestURLIncludesPurchaseMemo wire-pins the Solana Pay URL's
// #713 memo field: the wallet turns it into an SPL Memo instruction placed
// BEFORE the transfer, stamping the checkout session id on the wallet-built tx.
func TestTransferRequestURLIncludesPurchaseMemo(t *testing.T) {
	t.Parallel()

	const (
		recipient = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
		usdcMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
		reference = "11111111111111111111111111111112"
	)
	sessionID := uuid.MustParse("0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42")

	s := &SolanaPayService{}

	got := s.buildTransferRequestURL(context.Background(), recipient, 5_000_000, 6, usdcMint, "USDC", reference, solanarpc.PurchaseMemo(sessionID))
	require.Equal(t,
		"solana:"+recipient+
			"?amount=5.000000"+
			"&spl-token="+usdcMint+
			"&reference="+reference+
			"&memo=openrails%3A1%3A0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42"+
			"&label=Purchase",
		got)

	// No memo (defensive: e.g. an empty stamp) omits the param entirely.
	got = s.buildTransferRequestURL(context.Background(), recipient, 5_000_000, 6, usdcMint, "USDC", reference, "")
	require.NotContains(t, got, "memo=")
}
