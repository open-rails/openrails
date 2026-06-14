package riverjobs

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/db"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	// KindSolanaGasAlert is the cranker-wallet SOL float monitor (#258). NOT a
	// fee-payer / auto-top-up system — just an alert so ops keeps the wallet
	// funded. A pull that fails for lack of SOL is operational (retry), never
	// subscriber dunning.
	KindSolanaGasAlert = "openrails.solana_gas_alert"

	// defaultGasAlertThresholdLamports ~0.05 SOL. Each crank costs ~5000 lamports,
	// so this is ~10k cranks of runway.
	defaultGasAlertThresholdLamports = 50_000_000
)

// SolanaGasAlertArgs triggers a cranker-wallet SOL balance sweep.
type SolanaGasAlertArgs struct{}

func (SolanaGasAlertArgs) Kind() string { return KindSolanaGasAlert }

// solanaBalanceChecker is the minimal RPC surface (satisfied by *solana.RPCClient).
type solanaBalanceChecker interface {
	GetBalance(ctx context.Context, address solanago.PublicKey) (uint64, error)
}

// SolanaGasAlertWorker warns when a tenant's cranker (merchant) wallet — the one
// that signs + pays gas for transfer_subscription pulls — drops below the SOL
// threshold, so it can be topped up before pulls start failing for lack of gas.
type SolanaGasAlertWorker struct {
	river.WorkerDefaults[SolanaGasAlertArgs]
	DB                *db.DB
	RPC               solanaBalanceChecker
	ThresholdLamports uint64
}

func (SolanaGasAlertWorker) Kind() string { return KindSolanaGasAlert }

func (w *SolanaGasAlertWorker) Work(ctx context.Context, _ *river.Job[SolanaGasAlertArgs]) error {
	if w.RPC == nil || w.DB == nil {
		log.WithContext(ctx).Warn("Solana gas-alert worker not wired (no RPC/DB); skipping")
		return nil
	}
	threshold := w.ThresholdLamports
	if threshold == 0 {
		threshold = defaultGasAlertThresholdLamports
	}

	wallets, err := dbrepo.NewSolanaSubscriptionRepo(w.DB).ListActiveMerchantWallets(ctx)
	if err != nil {
		return fmt.Errorf("solana gas alert: list cranker wallets: %w", err)
	}
	for _, mw := range wallets {
		addr, err := solanago.PublicKeyFromBase58(mw.MerchantAddress)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("wallet", mw.MerchantAddress).
				Warn("Solana gas alert: invalid cranker wallet address")
			continue
		}
		bal, err := w.RPC.GetBalance(ctx, addr)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithField("wallet", mw.MerchantAddress).
				Warn("Solana gas alert: failed to read cranker wallet balance")
			continue
		}
		if bal < threshold {
			log.WithContext(ctx).WithFields(log.Fields{
				"tenant_id":          mw.MerchantID,
				"cranker_wallet":     mw.MerchantAddress,
				"balance_lamports":   bal,
				"threshold_lamports": threshold,
			}).Warn("Solana cranker wallet LOW on SOL gas — top up to avoid pull failures (#258)")
		}
	}
	return nil
}
