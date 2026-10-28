package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
)

// SolanaCrankerBalances checks the publisher before its first subscription and
// each owner still responsible for renewals, on the obligation's own PSP.
// Low balances are results; failed reads are errors, with other results retained.
func (r *Runtime) SolanaCrankerBalances(ctx context.Context) ([]solanamodule.CrankerBalance, error) {
	if r == nil || r.DB == nil {
		return nil, nil
	}
	ids := r.DeclaredMerchants()
	if mid, ok := merchant.FromContext(ctx); ok {
		ids = []billing.MerchantID{mid}
	} else if len(ids) == 0 {
		all, err := r.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("solana balances: list merchants: %w", err)
		}
		for _, id := range all {
			ids = append(ids, billing.MerchantID(id))
		}
	}
	var balances []solanamodule.CrankerBalance
	var errs []error
	for _, mid := range ids {
		if err := ctx.Err(); err != nil {
			return balances, errors.Join(append(errs, err)...)
		}
		scope := merchant.WithID(ctx, mid)
		got, err := r.merchantCrankerBalances(scope, mid)
		balances = append(balances, got...)
		if err != nil {
			errs = append(errs, fmt.Errorf("solana balances for merchant %s: %w", mid, err))
		}
	}
	return balances, errors.Join(errs...)
}

func (r *Runtime) merchantCrankerBalances(ctx context.Context, mid billing.MerchantID) ([]solanamodule.CrankerBalance, error) {
	owners, err := solanasubs.NewSolanaSubscriptionRepo(r.DB).ListActiveMerchantWallets(ctx)
	if err != nil {
		return nil, err
	}
	type target struct {
		psp    uuid.UUID
		wallet solanago.PublicKey
	}
	var targets []target
	seen := make(map[target]bool)
	add := func(t target) {
		if !seen[t] {
			targets = append(targets, t)
			seen[t] = true
		}
	}
	var errs []error
	for _, owner := range owners {
		wallet, err := solanago.PublicKeyFromBase58(owner.MerchantAddress)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid cranker %q: %w", owner.MerchantAddress, err))
			continue
		}
		add(target{psp: owner.PSPID, wallet: wallet})
	}
	if r.SolanaCrankers.Len() > 0 {
		if r.Merchants == nil {
			return nil, errors.New("merchant configuration unavailable")
		}
		psp, ok, err := r.Merchants.ActivePSPScope(ctx, mid, "solana")
		if err != nil && !errors.Is(err, merchants.ErrNoActivePSP) {
			errs = append(errs, err)
		}
		if ok {
			_, wallet, err := r.SolanaCrankers.Publisher(ctx)
			if err != nil {
				errs = append(errs, err)
			} else {
				add(target{psp: psp.ID, wallet: wallet})
			}
		}
	}
	var balances []solanamodule.CrankerBalance
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return balances, errors.Join(append(errs, err)...)
		}
		// A monitoring read is bounded separately from payment execution. The host's
		// shorter probe deadline still wins; a failed target does not stop the sweep.
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		rpc, network, err := r.SolanaRPCResolver.ResolvePSP(readCtx, mid, t.psp)
		var balance uint64
		if err == nil {
			now := time.Now()
			if r.Clock != nil {
				now = r.Clock.Now()
			}
			balance, err = r.solanaBalances.Read(readCtx, rpc, mid.UUID(), t.psp, t.wallet, now)
		}
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("cranker %s network %q PSP %s: balance could not be checked: %w", t.wallet, network, t.psp, err))
			continue
		}
		balances = append(balances, solanamodule.CrankerBalance{MerchantID: mid, PSPID: t.psp, Wallet: t.wallet, Network: network, Lamports: balance})
	}
	return balances, errors.Join(errs...)
}

// checkSolanaCrankerGas raises or resolves the low-gas finding of each cranker
// wallet that signs mid's pulls, after one signed. A wallet whose balance
// cannot be read keeps its finding.
func (r *Runtime) checkSolanaCrankerGas(ctx context.Context, mid billing.MerchantID) {
	// The chain reads hold no connection; only the findings do.
	balances, err := r.SolanaCrankerBalances(merchant.WithID(ctx, mid))
	if len(balances) > 0 {
		now := time.Now()
		if r.Clock != nil {
			now = r.Clock.Now()
		}
		err = errors.Join(err, r.DB.RunInMerchantScope(ctx, mid, "solana cranker gas", func(mctx context.Context) error {
			var errs error
			for _, b := range balances {
				errs = errors.Join(errs, alerting.RecordCrankerGas(mctx, r.DB, now, alerting.CrankerGas{
					PSPID: b.PSPID, Wallet: b.Wallet.String(), Network: b.Network, Lamports: b.Lamports, Threshold: solanamodule.CrankerWarningLamports}))
			}
			return errs
		}))
	}
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("merchant_id", mid.String()).Warn("Solana cranker: gas check failed")
	}
}
