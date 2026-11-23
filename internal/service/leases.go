package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/leases"
)

// SyncLeases syncs one gate's lease in one transaction: its usage settles in
// order, each want is answered with a hold or a refusal, the holds it returns
// are taken back and the rest are sized from its traffic. allows is the
// credential's customer scope.
func (s *Service) SyncLeases(ctx context.Context, params billing.SyncLeasesParams, allows func(billing.CustomerID) bool) (*billing.SyncLeasesResult, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	gate := spendgate.New(s.rt.DB)
	gate.SetClock(s.now)
	loader := admission.NewSpendgatePolicyLoader(admission.NewBillingPolicyStore(s.rt.DB), s.rt.FXProvider)
	adm := admission.NewAdmitter(s.moneyService(), gate, loader).
		WithFailedUsageCutoff(failedUsageCutoff{s}).
		WithDelinquency(s.delinquencyService()).
		WithAccrualRateMeter(admission.NewAccrualRateMeter(s.rt.DB))
	return leases.Syncer{DB: s.rt.DB, Money: s.moneyService(), Admitter: adm, FailedWindows: s.failedUsageWindows, Allows: allows, Now: s.now}.Sync(ctx, params)
}
