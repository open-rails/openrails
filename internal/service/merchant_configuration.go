package service

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// ErrRevisionMismatch refuses an edit naming a revision the configuration has
// moved past.
var ErrRevisionMismatch = merchants.ErrRevisionMismatch

func (s *Service) merchantConfig() (*merchantdocs.Cache, error) {
	if s == nil || s.rt == nil || s.rt.MerchantConfig == nil {
		return nil, errors.New("merchant configuration is not wired")
	}
	return s.rt.MerchantConfig, nil
}

// GetMerchantConfigurationState reads the merchant's configuration document,
// never its credentials.
func (s *Service) GetMerchantConfigurationState(ctx context.Context) (*billing.MerchantConfigurationState, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cache, err := s.merchantConfig()
	if err != nil {
		return nil, err
	}
	set, err := cache.Get(ctx, mid)
	if err != nil {
		return nil, err
	}
	return s.configurationState(ctx, mid, set)
}

func (s *Service) configurationState(ctx context.Context, mid billing.MerchantID, set merchantdocs.Set) (*billing.MerchantConfigurationState, error) {
	err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		_, err := s.rt.DB.Gen(ctx).GetMerchantDirectoryByID(ctx, mid.UUID())
		if errors.Is(err, pgx.ErrNoRows) {
			return billing.ErrMerchantNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &billing.MerchantConfigurationState{
		Revision: set.Merchant.Revision, DisplayName: set.Merchant.Value.DisplayName, MerchantSettings: set.Merchant.Value.MerchantSettings,
		PlatformVault: billing.PlatformVaultState{Available: s.rt.Merchants.PlatformVaultAvailable()},
	}, nil
}

// UpdateMerchantConfiguration merges params into the merchant document at the
// revision it names; without one, a concurrent edit is merged over.
func (s *Service) UpdateMerchantConfiguration(ctx context.Context, params billing.UpdateMerchantConfigurationParams) (*billing.MerchantConfigurationState, error) {
	if params.DisplayName != nil && strings.TrimSpace(*params.DisplayName) == "" {
		return nil, apperr.Invalidf("display_name must not be empty").WithParam("display_name")
	}
	if params.ExpectedRevision != nil && *params.ExpectedRevision < 0 {
		return nil, apperr.Invalidf("expected_revision must not be negative").WithParam("expected_revision")
	}
	if err := s.refuseUsageSettings(params.MerchantSettings); err != nil {
		return nil, err
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cache, err := s.merchantConfig()
	if err != nil {
		return nil, err
	}
	if !cache.Writable() {
		return nil, merchants.ErrConfigReadOnly
	}
	if params.MerchantSettings != nil && (params.MerchantSettings.UsageBilling != nil || params.MerchantSettings.BillingPolicies != nil || params.MerchantSettings.BillingPolicyBindings != nil || params.MerchantSettings.TrustTiers != nil) {
		unlock, err := s.rt.DB.LockUsagePolicy(ctx, mid)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	terms := params
	terms.ExpectedRevision = nil
	ctx, finishEdit, err := cache.BeginEdit(ctx, terms)
	if err != nil {
		return nil, err
	}
	defer finishEdit()
	for attempt := 0; ; attempt++ {
		set, err := cache.Reload(ctx, mid)
		if err != nil {
			return nil, err
		}
		held := set.Merchant
		cas := held.Revision
		if params.ExpectedRevision != nil {
			cas = *params.ExpectedRevision
		}
		next := held.Value
		if params.MerchantSettings != nil {
			next.MerchantSettings = mergeMerchantSettings(next.MerchantSettings, *params.MerchantSettings)
		}
		if params.DisplayName != nil {
			next.DisplayName = strings.TrimSpace(*params.DisplayName)
		}
		settings, err := merchantconfig.Normalize(next.DisplayName, next.MerchantSettings)
		var refusal *apperr.Error
		if errors.As(err, &refusal) {
			return nil, err
		}
		if err != nil {
			return nil, apperr.Invalidf("%s", err.Error()).WithParam("settings")
		}
		if err := s.refuseAssignedPolicyRemoval(ctx, mid, settings); err != nil {
			return nil, err
		}
		if usage := settings.Config.UsageBilling; usage != nil {
			for _, cur := range usage.Currencies {
				err := s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
					customer, err := s.rt.DB.Gen(ctx).FindInvalidUsageMinimum(ctx, gen.FindInvalidUsageMinimumParams{MerchantID: mid.UUID(), Currency: string(cur.Currency), DefaultCredit: cur.CreditLimit, DefaultMinimum: cur.MinimumInvoiceAmount, Policies: settings.PoliciesParam()})
					if errors.Is(err, pgx.ErrNoRows) {
						return nil
					}
					if err != nil {
						return err
					}
					return apperr.Invalidf("customer %s has a minimum invoice above its resulting %s credit limit", customer, cur.Currency).WithParam("settings.usage_billing")
				})
				if err != nil {
					return nil, err
				}
			}
		}
		if err := s.rt.Merchants.CheckCardVault(ctx, mid, next, set.PSPs); err != nil {
			return nil, err
		}
		put := func(ctx context.Context) (merchantdocs.Set, error) { return cache.PutMerchant(ctx, mid, next, cas) }
		if changed := merchantconfig.ChangedUnits(held.Value.MerchantSettings.Currencies, settings.Currencies); len(changed) > 0 {
			set, err = s.putUnitChange(ctx, mid, changed, put)
		} else {
			set, err = put(ctx)
		}
		if errors.Is(err, merchantdocs.ErrRevisionMismatch) {
			if params.ExpectedRevision != nil || attempt >= 2 {
				return nil, ErrRevisionMismatch
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		s.rt.MerchantChanged(mid)
		if why, refused := set.Rejected[merchantdocs.MerchantDoc]; refused {
			return nil, apperr.Conflictf("the configuration was written but is not served: %s", why)
		}
		if why, kept := set.Rejected[merchantconfig.UnitsDoc]; kept {
			return nil, apperr.Conflictf("the configuration was written, but %s", why)
		}
		return s.configurationState(ctx, mid, set)
	}
}

// refuseAssignedPolicyRemoval refuses settings that no longer declare a
// billing policy customers are assigned.
func (s *Service) refuseAssignedPolicyRemoval(ctx context.Context, mid billing.MerchantID, settings merchantconfig.Settings) error {
	names := make([]string, 0, len(settings.Policies))
	for name := range settings.Policies {
		names = append(names, name)
	}
	return s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		missing, err := s.rt.DB.Gen(ctx).FindAssignedBillingPolicyOutside(ctx, gen.FindAssignedBillingPolicyOutsideParams{MerchantID: mid.UUID(), Names: names})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return apperr.Invalidf("billing policy %q is assigned to customers; reassign them first", missing).WithParam("settings.billing_policies")
	})
}

// putUnitChange writes settings that remove or rescale units under the
// units lock every catalog write takes, refusing them while a balance, price,
// credit policy or rate card counts one of changed.
func (s *Service) putUnitChange(ctx context.Context, mid billing.MerchantID, changed []string, put func(context.Context) (merchantdocs.Set, error)) (merchantdocs.Set, error) {
	var set merchantdocs.Set
	err := s.rt.DB.MerchantTx(ctx, func(txCtx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.LockCatalogKey(txCtx, ledger.UnitsLock(mid.UUID())); err != nil {
			return err
		}
		inUse, err := merchantconfig.UnitsInUse(txCtx, q, mid.UUID())
		if err != nil {
			return err
		}
		for _, code := range changed {
			if slices.Contains(inUse, code) {
				return apperr.Invalidf("unit %s counts balances, prices, credit or rate cards: it cannot be removed or rescaled", code).WithParam("settings.currencies")
			}
		}
		set, err = put(ctx)
		return err
	})
	return set, err
}

func mergeMerchantSettings(current, patch billing.MerchantSettings) billing.MerchantSettings {
	if patch.Profile != nil {
		p := billing.MerchantProfile{}
		if current.Profile != nil {
			p = *current.Profile
		}
		if patch.Profile.LogoURL != "" {
			p.LogoURL = patch.Profile.LogoURL
		}
		if patch.Profile.FromEmail != "" {
			p.FromEmail = patch.Profile.FromEmail
		}
		if patch.Profile.SupportURL != "" {
			p.SupportURL = patch.Profile.SupportURL
		}
		if patch.Profile.SubscribeURL != "" {
			p.SubscribeURL = patch.Profile.SubscribeURL
		}
		if patch.Profile.BillingURL != "" {
			p.BillingURL = patch.Profile.BillingURL
		}
		if patch.Profile.TermsURL != "" {
			p.TermsURL = patch.Profile.TermsURL
		}
		if patch.Profile.PrivacyURL != "" {
			p.PrivacyURL = patch.Profile.PrivacyURL
		}
		if patch.Profile.StatementDescriptor != "" {
			p.StatementDescriptor = patch.Profile.StatementDescriptor
		}
		current.Profile = &p
	}
	if patch.UsageBilling != nil {
		current.UsageBilling = patch.UsageBilling
	}
	if patch.BillingPeriodBoundary != "" {
		current.BillingPeriodBoundary = patch.BillingPeriodBoundary
	}
	if patch.CardVault != "" {
		current.CardVault = patch.CardVault
	}
	if patch.ChargePSPs != nil {
		current.ChargePSPs = patch.ChargePSPs
	}
	if patch.AlertEmail != nil {
		current.AlertEmail = patch.AlertEmail
	}
	if patch.RepriceNoticeWindowDays != nil {
		current.RepriceNoticeWindowDays = patch.RepriceNoticeWindowDays
	}
	if patch.ProviderRefundAccess != nil {
		current.ProviderRefundAccess = patch.ProviderRefundAccess
	}
	if patch.FraudWarningRefund != nil {
		current.FraudWarningRefund = patch.FraudWarningRefund
	}
	if patch.ArrearsGraceDays != nil {
		current.ArrearsGraceDays = patch.ArrearsGraceDays
	}
	if patch.PSPRouting != nil {
		current.PSPRouting = patch.PSPRouting
	}
	if patch.DunningPolicy != nil {
		current.DunningPolicy = patch.DunningPolicy
	}
	if patch.Checkout != nil {
		current.Checkout = patch.Checkout
	}
	if patch.Accounting != nil {
		current.Accounting = patch.Accounting
	}
	if patch.BalanceNotices != nil {
		current.BalanceNotices = patch.BalanceNotices
	}
	if patch.BillingPolicies != nil {
		current.BillingPolicies = patch.BillingPolicies
	}
	if patch.BillingPolicyBindings != nil {
		current.BillingPolicyBindings = patch.BillingPolicyBindings
	}
	if patch.TrustTiers != nil {
		current.TrustTiers = patch.TrustTiers
	}
	if patch.DelegatedInvokerWastedSpendLimits != nil {
		current.DelegatedInvokerWastedSpendLimits = patch.DelegatedInvokerWastedSpendLimits
	}
	if patch.Currencies != nil {
		current.Currencies = patch.Currencies
	}
	return current
}
