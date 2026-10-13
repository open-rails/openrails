package app

import (
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/solana/solanasubs"
)

// ArmSolanaRecurringServices assembles the recurring Solana runtime from the
// merchant secret plane. Both standalone and embedded composition roots call
// this after merchant secrets are available.
func (r *Runtime) ArmSolanaRecurringServices(
	secretStore merchants.MerchantSecretReader,
	solanaTransit solanaint.TransitClient,
) {
	if r == nil || r.DB == nil || r.Config == nil || r.SolanaRPCResolver == nil {
		return
	}

	solanaSigner := recurring.NewSignerFromPSPs(secretStore, solanaTransit, r.DB, 0)
	submitter := recurring.NewSignerSubmitterWithResolver(solanaSigner, r.SolanaRPCResolver.Resolve)
	// or#881: a mint LOOKUP table for the recurring plan services, NOT an
	// acceptance list: the built-in mints of the network the merchant's
	// Solana PSP pays on. What a merchant accepts is resolved by
	// tokens.ResolveDeclared; plan publishing is gated by the recurring
	// allowlist (recurring.RecurringStablecoins).
	tokens := recurring.Tokens(r.SolanaRPCResolver.RecurringTokens)
	r.SetSolanaCranker(recurring.NewCrankService(submitter))

	if r.SubscriptionLifecycleService == nil {
		return
	}
	chainReader := r.SolanaRPCResolver.ChainReader()
	subscriptions := solanasubs.NewSolanaSubscriptionRepo(r.DB)
	plan := recurring.NewPlanServiceWithReader(submitter, chainReader, tokens)
	enroll := recurring.NewEnrollService(chainReader, submitter, tokens)
	r.SetSolanaPlanService(plan)
	r.SetSolanaPrepareCancelService(recurring.NewPrepareCancelService(
		subscriptions,
		chainReader,
	))
	r.SetSolanaPrepareTierChangeService(recurring.NewPrepareTierChangeService(
		solanaSigner,
		chainReader,
		tokens,
	))

	if r.OrderAttemptService == nil {
		return
	}
	prepare := recurring.NewPrepareSubscribeService(
		submitter,
		solanaSigner,
		chainReader,
		tokens,
	)
	r.OrderAttemptService.SetSolanaRecurring(prepare, enroll)
}
