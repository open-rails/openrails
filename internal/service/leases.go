package service

import (
	"context"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/leases"
)

// SyncLeases syncs one gate's lease in one transaction: its usage settles in
// order and its in-flight report is replaced; each view it names is answered
// once it committed. allows is the credential's customer scope.
func (s *Service) SyncLeases(ctx context.Context, params billing.SyncLeasesParams, allows func(billing.CustomerID) bool) (*billing.SyncLeasesResult, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	loader := admission.NewSpendgatePolicyLoader(admission.NewBillingPolicyStore(s.rt.DB), s.rt.FXProvider)
	adm := admission.NewAdmitter(s.moneyService(), loader, s.now).
		WithFailedUsageCutoff(failedUsageCutoff{s}).
		WithDelinquency(s.delinquencyService())
	return leases.Syncer{DB: s.rt.DB, Money: s.moneyService(), Admitter: adm, FailedWindows: s.failedUsageWindows, Allows: allows, Now: s.now}.Sync(ctx, params)
}
