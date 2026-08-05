package app

import (
	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// ArmSolanaRecurringServices assembles the recurring Solana runtime from the
// merchant secret plane. Both standalone and embedded composition roots call
// this after merchant secrets are available.
func (r *Runtime) ArmSolanaRecurringServices(
	secretStore merchants.MerchantSecretStore,
	solanaTransit solanaint.TransitClient,
) {
	if r == nil || r.DB == nil || r.Config == nil || r.SolanaRPCResolver == nil {
		return
	}

	solanaSigner := recurring.NewSignerFromRailMerchantAccounts(
		secretStore,
		solanaTransit,
		r.DB,
		0,
		config.ExpectedProviderEnvironment(r.Config.IsTestMode()),
	)
	submitter := recurring.NewSignerSubmitterWithResolver(solanaSigner, r.SolanaRPCResolver.Resolve)
	network := "mainnet"
	if r.Config.IsTestMode() {
		network = "devnet"
	}
	// or#881: a mint LOOKUP table for the recurring plan services, NOT an
	// acceptance list. What a merchant accepts is per-merchant and resolved by
	// tokens.ResolveDeclared; plan publishing is separately gated by the
	// recurring allowlist (recurring.RecurringStablecoins).
	tokens := solanatokens.ForNetwork(network)
	r.SetSolanaCranker(recurring.NewCrankService(submitter))

	if r.SubscriptionLifecycleService == nil {
		return
	}
	chainReader := r.SolanaRPCResolver.ChainReader()
	subscriptions := solanasubs.NewSolanaSubscriptionRepo(r.DB)
	plan := recurring.NewPlanServiceWithReader(submitter, chainReader, network, tokens)
	enroll := recurring.NewEnrollService(
		r.SubscriptionLifecycleService,
		subscriptions,
		chainReader,
		submitter,
		network,
		tokens,
	)
	r.SetSolanaRecurringServices(plan, enroll)
	r.SetSolanaPrepareCancelService(recurring.NewPrepareCancelService(
		subscriptions,
		chainReader,
	))
	r.SetSolanaPrepareTierChangeService(recurring.NewPrepareTierChangeService(
		solanaSigner,
		chainReader,
		network,
		tokens,
	))

	if r.CheckoutSessionService == nil {
		return
	}
	prepare := recurring.NewPrepareSubscribeService(
		submitter,
		solanaSigner,
		chainReader,
		network,
		tokens,
	)
	r.CheckoutSessionService.SetSolanaRecurring(prepare, enroll)
	confirmCancel := recurring.NewConfirmCancelService(
		chainReader,
		r.SubscriptionLifecycleService,
	)
	confirmTierChange := recurring.NewConfirmTierChangeService(
		chainReader,
		r.SubscriptionLifecycleService,
		subscriptions,
		network,
		tokens,
	)
	r.CheckoutSessionService.SetSolanaLifecycle(
		r.SolanaPrepareCancelService,
		r.SolanaPrepareTierChangeService,
		confirmCancel,
		confirmTierChange,
		r.SubscriptionService,
		subscriptions,
	)
}
