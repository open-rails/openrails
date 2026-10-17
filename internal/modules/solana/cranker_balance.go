package solana

import (
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// CrankerWarningLamports is a funding warning, not an estimate of how many
// transactions a wallet can still pay for.
const CrankerWarningLamports uint64 = 50_000_000

type CrankerBalance struct {
	MerchantID billing.MerchantID
	PSPID      uuid.UUID
	Wallet     solanago.PublicKey
	Network    string
	Lamports   uint64
}

func (b CrankerBalance) Low() bool { return b.Lamports < CrankerWarningLamports }

func (b CrankerBalance) String() string {
	return fmt.Sprintf("Solana cranker %s on %s for merchant %s (PSP %s) has %s; funding warning below %s",
		b.Wallet, b.Network, b.MerchantID, b.PSPID, moneyutil.FormatBaseUnits(b.Lamports, "SOL"), moneyutil.FormatBaseUnits(CrankerWarningLamports, "SOL"))
}
