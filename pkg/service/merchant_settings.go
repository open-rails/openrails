package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

var ErrInvalidMerchantSettings = errors.New("invalid merchant settings")

type merchantSettingsDocument struct {
	config    models.MerchantConfiguration
	policies  map[string]models.BillingPolicy
	schedules []openrails.MerchantTrustLevelSchedule
	bindings  []openrails.BillingPolicyBindingInput
}

// normalizeMerchantSettings validates the complete declaration before any write.
func (s *Service) normalizeMerchantSettings(ctx context.Context, in openrails.MerchantSettings) (merchantSettingsDocument, error) {
	doc := merchantSettingsDocument{policies: make(map[string]models.BillingPolicy)}
	declaredWindows, err := merchantconfig.NormalizeBudgetWindows("merchant settings", "delegated_invoker_wasted_spend_limits", budgetScopeWindowModels(in.DelegatedInvokerWastedSpendLimits))
	if err != nil {
		return doc, err
	}
	windows := make([]abuse.WastedWindow, 0, len(declaredWindows))
	for _, w := range declaredWindows {
		windows = append(windows, abuse.WastedWindow{Key: w.Key, Window: time.Duration(w.WindowSeconds) * time.Second, Limit: w.Limit, Currency: w.Currency})
	}
	var profile *models.MerchantProfileConfiguration
	if in.Profile != nil {
		profile = &models.MerchantProfileConfiguration{DisplayName: strings.TrimSpace(in.Profile.DisplayName), LogoURL: strings.TrimSpace(in.Profile.LogoURL), FromEmail: strings.TrimSpace(in.Profile.FromEmail), SupportURL: strings.TrimSpace(in.Profile.SupportURL), SignupURL: strings.TrimSpace(in.Profile.SignupURL)}
	}
	doc.config, err = applyMerchantConfiguration(models.MerchantConfiguration{}, MerchantConfiguration{
		Profile: profile, AutoTopupSafety: in.AutoTopupSafety, InvoiceCollectionThreshold: in.InvoiceCollectionThreshold,
		InvoiceMonthlyFloor: in.InvoiceMonthlyFloor, InvoiceBillingBoundary: in.InvoiceBillingBoundary, AlertEmail: in.AlertEmail,
		RepriceNoticeWindowDays: in.RepriceNoticeWindowDays, ArrearsGraceDays: in.ArrearsGraceDays,
		ArrearsDelinquencyFloor: in.ArrearsDelinquencyFloor, CheckoutRouting: in.CheckoutRouting,
		DelegatedInvokerWastedSpendWindows: windows,
	})
	if err != nil {
		return doc, err
	}
	for _, policy := range in.BillingPolicies {
		name, body, err := ValidateBillingPolicy(policy)
		if err != nil {
			return doc, err
		}
		if _, exists := doc.policies[name]; exists {
			return doc, fmt.Errorf("duplicate billing policy %q", name)
		}
		doc.policies[name] = body
	}
	bound := make(map[string]bool)
	for _, binding := range in.BillingPolicyBindings {
		if binding.CustomerID != "" {
			return doc, fmt.Errorf("customer bindings are runtime state, outside merchant settings")
		}
		binding.Tier = strings.TrimSpace(binding.Tier)
		binding.PolicyName = strings.TrimSpace(binding.PolicyName)
		if _, exists := doc.policies[binding.PolicyName]; !exists {
			return doc, fmt.Errorf("binding names undeclared policy %q", binding.PolicyName)
		}
		if bound[binding.Tier] {
			return doc, fmt.Errorf("duplicate binding for tier %q", binding.Tier)
		}
		bound[binding.Tier] = true
		doc.bindings = append(doc.bindings, binding)
	}
	currencies := make(map[string]bool)
	for _, schedule := range in.TrustLevelSchedules {
		currency, err := s.resolveCurrency(ctx, schedule.Currency)
		if err != nil {
			return doc, err
		}
		if err := moneyutil.ValidateCurrency(currency); err != nil {
			return doc, err
		}
		if currencies[currency] {
			return doc, fmt.Errorf("duplicate trust schedule for %q", currency)
		}
		currencies[currency] = true
		schedule.Currency = currency
		schedule.Schedule = append([]openrails.TrustLevelScheduleRung{}, schedule.Schedule...)
		levels := make(map[string]bool)
		thresholds := make(map[int64]bool)
		for i := range schedule.Schedule {
			rung := &schedule.Schedule[i]
			rung.TrustLevel = strings.TrimSpace(rung.TrustLevel)
			if rung.TrustLevel == "" || rung.MinCumulativePaidAmount < 0 || levels[rung.TrustLevel] || thresholds[rung.MinCumulativePaidAmount] {
				return doc, fmt.Errorf("invalid or duplicate trust schedule rung")
			}
			levels[rung.TrustLevel], thresholds[rung.MinCumulativePaidAmount] = true, true
		}
		sort.Slice(schedule.Schedule, func(i, j int) bool {
			return schedule.Schedule[i].MinCumulativePaidAmount < schedule.Schedule[j].MinCumulativePaidAmount
		})
		doc.schedules = append(doc.schedules, schedule)
	}
	return doc, nil
}

// SetMerchantSettings atomically replaces the declarative merchant document.
// Customer-specific schedules and policy bindings remain runtime state. A policy
// referenced by such a binding cannot be removed by replacing the document.
func (s *Service) SetMerchantSettings(ctx context.Context, in openrails.MerchantSettings) error {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return err
	}
	defer release()
	doc, err := s.normalizeMerchantSettings(ctx, in)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrInvalidMerchantSettings, err)
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		database := s.rt.DB.NewWithPgxTx(tx)
		q := database.Gen(ctx)
		if _, err := q.LockMerchantSettings(ctx, mid.UUID()); err != nil {
			return err
		}
		names := make([]string, 0, len(doc.policies))
		for name := range doc.policies {
			names = append(names, name)
		}
		sort.Strings(names)
		removed, err := q.FindRemovedCustomerPolicies(ctx, gen.FindRemovedCustomerPoliciesParams{MerchantID: mid.UUID(), Names: names})
		if err != nil {
			return err
		}
		if len(removed) != 0 {
			return fmt.Errorf("%w: policies still bound to customers: %s", ErrInvalidMerchantSettings, strings.Join(removed, ", "))
		}
		if err := merchantconfig.NewStore(database).Upsert(ctx, doc.config); err != nil {
			return err
		}
		if err := q.DeleteDefaultTrustLevelSchedules(ctx, mid.UUID()); err != nil {
			return err
		}
		now := s.now()
		for _, schedule := range doc.schedules {
			rungs, err := json.Marshal(schedule.Schedule)
			if err != nil {
				return err
			}
			if err := q.UpsertTierScheduleDefault(ctx, gen.UpsertTierScheduleDefaultParams{ID: uuidutil.NewV7(), MerchantID: mid.UUID(), Currency: schedule.Currency, Rungs: rungs, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
		}
		if err := q.DeleteDeclarativeBillingPolicyBindings(ctx, mid.UUID()); err != nil {
			return err
		}
		policies := admission.NewBillingPolicyStore(database)
		for _, name := range names {
			if err := policies.UpsertPolicy(ctx, name, doc.policies[name]); err != nil {
				return err
			}
		}
		if err := q.DeleteUndeclaredBillingPolicies(ctx, gen.DeleteUndeclaredBillingPoliciesParams{MerchantID: mid.UUID(), Names: names}); err != nil {
			return err
		}
		for _, binding := range doc.bindings {
			if err := policies.BindPolicy(ctx, identity.CustomerID{}, binding.Tier, binding.PolicyName); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetMerchantSettings reads one complete declaration while excluding a concurrent
// replacement. Runtime customer segmentation is deliberately not enumerated.
func (s *Service) GetMerchantSettings(ctx context.Context) (out openrails.MerchantSettings, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return out, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		database := s.rt.DB.NewWithPgxTx(tx)
		q := database.Gen(ctx)
		if _, err := q.ReadMerchantSettingsLock(ctx, mid.UUID()); err != nil {
			return err
		}
		view := &Service{rt: &app.Runtime{DB: database}}
		cfg, _, err := view.GetMerchantConfiguration(ctx)
		if err != nil {
			return err
		}
		out = openrails.MerchantSettings{
			AutoTopupSafety: cfg.AutoTopupSafety, InvoiceCollectionThreshold: cfg.InvoiceCollectionThreshold,
			InvoiceMonthlyFloor: cfg.InvoiceMonthlyFloor, InvoiceBillingBoundary: cfg.InvoiceBillingBoundary, AlertEmail: cfg.AlertEmail,
			RepriceNoticeWindowDays: cfg.RepriceNoticeWindowDays, ArrearsGraceDays: cfg.ArrearsGraceDays,
			ArrearsDelinquencyFloor: cfg.ArrearsDelinquencyFloor, CheckoutRouting: cfg.CheckoutRouting,
		}
		if cfg.Profile != nil {
			out.Profile = &openrails.MerchantProfileInput{DisplayName: cfg.Profile.DisplayName, LogoURL: cfg.Profile.LogoURL, FromEmail: cfg.Profile.FromEmail, SupportURL: cfg.Profile.SupportURL, SignupURL: cfg.Profile.SignupURL}
		}
		for _, w := range cfg.DelegatedInvokerWastedSpendWindows {
			out.DelegatedInvokerWastedSpendLimits = append(out.DelegatedInvokerWastedSpendLimits, openrails.BudgetWindowInput{Key: w.Key, WindowSeconds: int64(w.Window / time.Second), Limit: w.Limit, Currency: w.Currency})
		}
		out.BillingPolicies, err = view.ListBillingPolicies(ctx)
		if err != nil {
			return err
		}
		out.BillingPolicyBindings, err = view.ListBillingPolicyBindings(ctx)
		if err != nil {
			return err
		}
		schedules, err := q.ListDefaultTrustLevelSchedules(ctx, mid.UUID())
		if err != nil {
			return err
		}
		for _, row := range schedules {
			schedule := openrails.MerchantTrustLevelSchedule{Currency: row.Currency}
			if err := json.Unmarshal(row.Rungs, &schedule.Schedule); err != nil {
				return err
			}
			out.TrustLevelSchedules = append(out.TrustLevelSchedules, schedule)
		}
		return nil
	})
	return out, err
}
