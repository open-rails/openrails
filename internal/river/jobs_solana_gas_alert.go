package riverjobs

import (
	"context"
	"errors"

	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindSolanaGasAlert = "openrails.solana_gas_alert"

type SolanaGasAlertArgs struct{}

func (SolanaGasAlertArgs) Kind() string { return KindSolanaGasAlert }

// SolanaGasAlertWorker uses the same balance reads as the optional health
// probe. A low balance warns; failed reads return an error for normal job retry.
type SolanaGasAlertWorker struct {
	river.WorkerDefaults[SolanaGasAlertArgs]
	Check func(context.Context) ([]solanamodule.CrankerBalance, error)
}

func (SolanaGasAlertWorker) Kind() string { return KindSolanaGasAlert }

func (w *SolanaGasAlertWorker) Work(ctx context.Context, _ *river.Job[SolanaGasAlertArgs]) error {
	if w.Check == nil {
		return errors.New("Solana gas alert: balance checker unavailable")
	}
	balances, err := w.Check(ctx)
	for _, balance := range balances {
		if balance.Low() {
			log.WithContext(ctx).WithFields(log.Fields{
				"tenant_id": balance.MerchantID, "psp_id": balance.PSPID,
				"cranker_wallet": balance.Wallet.String(), "network": balance.Network,
				"balance":   moneyutil.FormatBaseUnits(balance.Lamports, "SOL"),
				"threshold": moneyutil.FormatBaseUnits(solanamodule.CrankerWarningLamports, "SOL"),
			}).Warn("Solana cranker wallet LOW on SOL gas — top up to avoid pull failures (#258)")
		}
	}
	return err
}
