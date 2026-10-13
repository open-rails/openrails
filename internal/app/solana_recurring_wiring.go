package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
)

// ArmSolanaRecurringServices assembles Solana rebilling around the
// deployment's listed crankers. With none, Solana rebilling is off: no plan is
// published, no recurring price is offered on Solana and nothing is pulled;
// one-time Solana payments still work. Both composition roots call this once
// merchant configuration is available.
func (r *Runtime) ArmSolanaRecurringServices(transit solanaint.TransitClient) {
	if r == nil || r.DB == nil || r.Config == nil || r.SolanaRPCResolver == nil {
		return
	}
	crankers, err := recurring.BuildCrankers(config.SolanaCrankers(r.Config), transit, CrankerPins{DB: r.DB})
	if err != nil {
		log.WithError(err).Error("solana: the crankers are not armed; Solana rebilling is off")
		return
	}
	if crankers.Len() == 0 {
		log.Info("solana: no solana.crankers; Solana rebilling is off, one-time Solana payments work")
		return
	}
	r.SolanaCrankers = crankers
	submitter := recurring.NewSignerSubmitter(crankers, r.SolanaRPCResolver.Resolve)
	// The mint lookup of the network each merchant's Solana PSP pays on.
	tokens := recurring.Tokens(r.SolanaRPCResolver.RecurringTokens)
	r.SetSolanaCranker(recurring.NewCrankService(submitter))

	if r.SubscriptionLifecycleService == nil {
		return
	}
	chainReader := r.SolanaRPCResolver.ChainReader()
	subscriptions := solanasubs.NewSolanaSubscriptionRepo(r.DB)
	plan := recurring.NewPlanServiceWithReader(submitter, chainReader, tokens, r.SolanaRPCResolver.Treasury)
	enroll := recurring.NewEnrollService(chainReader, tokens)
	r.SetSolanaPlanService(plan)
	r.SetSolanaPrepareCancelService(recurring.NewPrepareCancelService(subscriptions, chainReader))
	r.SetSolanaPrepareTierChangeService(recurring.NewPrepareTierChangeService(submitter, chainReader, tokens))

	if r.OrderAttemptService == nil {
		return
	}
	prepare := recurring.NewPrepareSubscribeService(submitter, chainReader, tokens)
	r.OrderAttemptService.SetSolanaRecurring(prepare, enroll)
}

// CrankerPins pins each listed cranker in billing.solana_crankers.
type CrankerPins struct{ DB *db.DB }

// Pin records key for the named cranker unless it is pinned, and answers its
// pin.
func (p CrankerPins) Pin(ctx context.Context, name string, key solanago.PublicKey) (solanago.PublicKey, error) {
	pinned, err := p.DB.Gen(ctx).PinSolanaCranker(ctx, gen.PinSolanaCrankerParams{Name: name, PublicKey: key.String()})
	if err != nil {
		return solanago.PublicKey{}, err
	}
	return solanago.PublicKeyFromBase58(pinned)
}

// SolanaCrankersCheck warns while a listed cranker cannot answer for its key,
// or active Solana subscriptions are owned by a key no listed cranker holds:
// their renewals cannot be pulled. It never stops the engine.
func (r *Runtime) SolanaCrankersCheck(ctx context.Context) error {
	if r == nil || r.DB == nil {
		return nil
	}
	// A cranker whose key changed is not served until restored.
	for name, err := range r.SolanaCrankers.Changed(ctx) {
		if err != nil {
			r.Refuse("solana.crankers."+name, err)
		} else {
			r.Unrefuse("solana.crankers." + name)
		}
	}
	var held []solanago.PublicKey
	if r.SolanaCrankers != nil {
		keys, err := r.SolanaCrankers.Keys(ctx)
		if err != nil {
			return err
		}
		held = keys
	}
	owners, err := solanasubs.NewSolanaSubscriptionRepo(r.DB).ListActiveMerchantWallets(ctx)
	if err != nil {
		return fmt.Errorf("solana crankers: count subscriptions: %w", err)
	}
	var subs, plans int64
	var unheld []string
	for _, o := range owners {
		if slices.ContainsFunc(held, func(k solanago.PublicKey) bool { return k.String() == o.MerchantAddress }) {
			continue
		}
		subs, plans = subs+o.Subscriptions, plans+o.Plans
		if !slices.Contains(unheld, o.MerchantAddress) {
			unheld = append(unheld, o.MerchantAddress)
		}
	}
	if subs == 0 {
		return nil
	}
	slices.Sort(unheld)
	return fmt.Errorf("%w: %d active subscriptions on %d plans are owned by %s", recurring.ErrUnheldPlan, subs, plans, strings.Join(unheld, ", "))
}
