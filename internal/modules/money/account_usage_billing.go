package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// KindUsageOrders bills, notifies and collects usage orders.
const KindUsageOrders = "openrails.usage_orders"

// UsageOrdersInterval is the deployment-wide pass's cadence.
const UsageOrdersInterval = 5 * time.Minute

// usageOutstanding are a pass's states before it finishes.
var usageOutstanding = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
}

// UsageOrderArgs is one usage-order pass: every merchant's due accounts, or
// one merchant's after a threshold crossing. A merchant has at most one
// outstanding wakeup, rather than a timer or job for every customer.
type UsageOrderArgs struct {
	MerchantID *billing.MerchantID `json:"merchant_id,omitempty"`
}

func (UsageOrderArgs) Kind() string { return KindUsageOrders }

// InsertOpts coalesces the deployment-wide passes. A merchant's wake coalesces
// only where its producer asks (a threshold crossing).
func (a UsageOrderArgs) InsertOpts() river.InsertOpts {
	if a.MerchantID != nil {
		return river.InsertOpts{Queue: "billing"}
	}
	return river.InsertOpts{Queue: "billing", UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: usageOutstanding}}
}

func (s *MoneyService) usageBillingConfig(ctx context.Context, currency string) (*billing.UsageBillingSettings, *billing.UsageBillingCurrency, error) {
	if !s.UsageBillingEnabled {
		return nil, nil, nil
	}
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return nil, nil, err
	}
	if cfg.UsageBilling == nil {
		return nil, nil, nil
	}
	for i := range cfg.UsageBilling.Currencies {
		if cfg.UsageBilling.Currencies[i].Currency == currency {
			return cfg.UsageBilling, &cfg.UsageBilling.Currencies[i], nil
		}
	}
	return cfg.UsageBilling, nil, nil
}

func NextUsageBillingDate(now, anchor time.Time) time.Time {
	now, anchor = now.UTC(), anchor.UTC()
	next := anniversaryInMonth(now.Year(), now.Month(), anchor)
	if !next.After(now) {
		month := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		next = anniversaryInMonth(month.Year(), month.Month(), anchor)
	}
	return next
}

func (s *MoneyService) ensureUsageBillingAccount(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string, cfg *billing.UsageBillingSettings) (gen.BillingMoneySetting, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return gen.BillingMoneySetting{}, err
	}
	existing, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
	if err == nil && existing.UsageBillingAnchor != nil && existing.UsageNextBillAt != nil {
		return existing, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return existing, err
	}
	// January contains every valid day; its year is immaterial to recurrence.
	anchor := time.Date(2000, time.January, cfg.BillingDay, 0, 0, 0, 0, time.UTC)
	next := NextUsageBillingDate(s.now(), anchor)
	if first, err := q.EarliestPendingOwedAccrual(ctx, gen.EarliestPendingOwedAccrualParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency}); err == nil {
		next = NextUsageBillingDate(first, anchor)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return existing, err
	}
	return q.EnsureUsageBillingAccount(ctx, gen.EnsureUsageBillingAccountParams{
		MerchantID: mid.UUID(), CustomerID: customer, Currency: currency,
		Anchor: anchor, NextBillAt: next, Now: s.now(),
	})
}

func usageCreditLimit(row gen.BillingMoneySetting, cfg *billing.UsageBillingCurrency) int64 {
	if row.CreditLimitOverride {
		return row.CreditLimitAmount
	}
	if cfg == nil {
		return 0
	}
	return cfg.CreditLimit
}

func usageMinimumInvoice(row gen.BillingMoneySetting, cfg *billing.UsageBillingCurrency) int64 {
	if row.UsageMinimumInvoiceAmount != nil {
		return *row.UsageMinimumInvoiceAmount
	}
	if cfg == nil {
		return 0
	}
	return cfg.MinimumInvoiceAmount
}

// queueUsageBilling runs inside the accrual transaction. The due record is the
// source of work; the coalesced River job only wakes its processor.
func (s *MoneyService) queueUsageBilling(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string) error {
	return s.requestUsageBilling(ctx, q, customer, currency, false)
}

// RequestUsageCollection asks to collect an existing balance when a new request
// would exceed the credit limit. A denial commits this wakeup without reserving
// or charging the rejected work.
func (s *MoneyService) RequestUsageCollection(ctx context.Context, q *gen.Queries, customer identity.CustomerID, currency string) error {
	return s.requestUsageBilling(ctx, q, customer.UUID(), currency, true)
}

func (s *MoneyService) requestUsageBilling(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string, force bool) error {
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil || cur == nil {
		return err
	}
	row, err := s.ensureUsageBillingAccount(ctx, q, customer, currency, cfg)
	if err != nil {
		return err
	}
	terms, err := s.resolveUsageTerms(ctx, q, row, cur)
	if err != nil {
		return err
	}
	if row.UsageCollectionDue {
		return nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	owed, err := s.moneyLedger(q, mid.UUID()).OutstandingOwed(ctx, customer, currency)
	if err != nil {
		return err
	}
	if (!force && owed < terms.threshold) || owed <= 0 || owed < terms.minimum {
		return err
	}
	if err = q.FlagUsageCollectionDue(ctx, gen.FlagUsageCollectionDueParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency}); err != nil {
		return err
	}
	_, err = s.db.InsertRiverJob(ctx, UsageOrderArgs{MerchantID: &mid}, &river.InsertOpts{Queue: "billing", UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: usageOutstanding}})
	return err
}

// FinalizeDueUsage bills one bounded batch. Due rows survive process restarts;
// all decisions and attachments serialize with usage writes on the customer.
func (s *MoneyService) FinalizeDueUsage(ctx context.Context, now time.Time) (int, error) {
	if !s.UsageBillingEnabled {
		return 0, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, err
	}
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if err != nil {
		return 0, err
	}
	if settings.Config.UsageBilling == nil {
		return 0, nil
	}
	limits := map[string]int64{}
	minimums := map[string]int64{}
	for _, cur := range settings.Config.UsageBilling.Currencies {
		limits[cur.Currency] = cur.CreditLimit
		minimums[cur.Currency] = cur.MinimumInvoiceAmount
	}
	encoded, err := json.Marshal(limits)
	if err != nil {
		return 0, err
	}
	encodedMinimums, err := json.Marshal(minimums)
	if err != nil {
		return 0, err
	}
	count := 0
	var failures []error
	deadline := time.Now().Add(30 * time.Second)
	afterCustomer, afterCurrency := uuid.Nil, ""
	for {
		rows, err := s.db.Gen(ctx).ListUsageBillingDueAccounts(ctx, gen.ListUsageBillingDueAccountsParams{MerchantID: mid.UUID(), Now: now, RowLimit: 200, Limits: encoded, Minimums: encodedMinimums, Policies: settings.PoliciesParam(), AfterCustomer: afterCustomer, AfterCurrency: afterCurrency})
		if err != nil {
			return count, errors.Join(append(failures, err)...)
		}
		for _, row := range rows {
			afterCustomer, afterCurrency = row.CustomerID, row.Currency
			billed, err := s.finalizeUsageAccount(ctx, row.CustomerID, row.Currency, now)
			if err != nil {
				failures = append(failures, fmt.Errorf("usage account %s/%s: %w", row.CustomerID, row.Currency, err))
				if ctx.Err() != nil {
					return count, errors.Join(failures...)
				}
				continue
			}
			if billed {
				count++
			}
		}
		if len(rows) < 200 {
			break
		}
		if time.Now().After(deadline) {
			break
		}
	}
	return count, errors.Join(failures...)
}

func (s *MoneyService) finalizeUsageAccount(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil {
		return false, err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	billed := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
			return err
		}
		if cur == nil {
			// Removing a currency stops creating new invoices and clears its
			// stale wakeup; previously accepted payment operations still recover.
			return q.CompleteUsageBillingCheck(ctx, gen.CompleteUsageBillingCheckParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, NextBillAt: now.Add(24 * time.Hour), Now: now})
		}
		row, err := s.ensureUsageBillingAccount(ctx, q, customer, currency, cfg)
		if err != nil {
			return err
		}
		due := row.UsageNextBillAt != nil && !row.UsageNextBillAt.After(now)
		terms, err := s.resolveUsageTerms(ctx, q, row, cur)
		if err != nil {
			return err
		}
		owed, err := s.moneyLedger(q, mid.UUID()).OutstandingOwed(ctx, customer, currency)
		if err != nil {
			return err
		}
		next := *row.UsageNextBillAt
		if due {
			next = NextUsageBillingDate(now, *row.UsageBillingAnchor)
		}
		// Clear the wakeup in the same transaction as the invoice. A failed
		// invoice transaction leaves the account due for retry.
		if err := q.CompleteUsageBillingCheck(ctx, gen.CompleteUsageBillingCheckParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, NextBillAt: next, Now: now}); err != nil {
			return err
		}
		if !due && !row.UsageCollectionDue && owed < terms.threshold {
			return nil
		}
		from, err := q.EarliestPendingOwedAccrual(ctx, gen.EarliestPendingOwedAccrualParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		cutoff := now.Add(time.Microsecond)
		last, err := q.LatestUsageOrderEnd(ctx, gen.LatestUsageOrderEndParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, PeriodStart: from})
		if err != nil {
			return err
		}
		if !last.Before(cutoff) {
			cutoff = last.Add(time.Microsecond)
		}
		sources, err := q.SumPendingOwedItemAmountBySourceInPeriod(ctx, gen.SumPendingOwedItemAmountBySourceInPeriodParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, PeriodStartsAt: from, PeriodEndsAt: cutoff})
		if err != nil {
			return err
		}
		var pending int64
		for _, source := range sources {
			pending += source.Amount
		}
		claimed, err := q.SumOwedOrderClaims(ctx, gen.SumOwedOrderClaimsParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
		if err != nil {
			return err
		}
		amount := min(pending, max(owed-claimed, 0))
		if amount <= 0 || amount < terms.minimum {
			return nil
		}
		order, err := s.finalizeUsageTx(ctx, q, identity.CustomerID(customer), currency, from, cutoff)
		if err != nil {
			return fmt.Errorf("invoice usage: %w", err)
		}
		billed = order != nil
		return nil
	})
	return billed, err
}

func (s *MoneyService) usageAdmissionCapacity(ctx context.Context, q *gen.Queries, payer identity.CustomerID, currency string, capacity AdmissionCapacity) (AdmissionCapacity, error) {
	// Automatic usage collection is opt-in. Existing manual credit policies
	// keep their independent admission contract when it is not configured.
	if !s.UsageBillingEnabled {
		return capacity, nil
	}
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil {
		return capacity, err
	}
	if cfg == nil {
		return capacity, nil
	}
	if cur == nil {
		capacity.BillingMode = BillingModePrepaid
		capacity.CreditLimit = 0
		capacity.HardCreditLimit = true
		return capacity, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return capacity, err
	}
	row, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		row = gen.BillingMoneySetting{MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency}
	} else if err != nil {
		return capacity, err
	}
	terms, err := s.resolveUsageTerms(ctx, q, row, cur)
	if err != nil {
		return capacity, err
	}
	capacity.HardCreditLimit = true
	capacity.CreditLimit = terms.creditLimit
	if terms.creditLimit > 0 && terms.minimum > terms.creditLimit {
		return capacity, fmt.Errorf("usage minimum invoice exceeds the credit limit for %s", currency)
	}
	capacity.BillingMode = BillingModeArrears
	if row.CollectionRevokedAt != nil {
		capacity.CreditLimit = 0
		capacity.BillingMode = BillingModePrepaid
		return capacity, nil
	}
	method, err := s.collectionMethodFor(ctx, q, mid.UUID(), payer.UUID(), currency, usageCollectionEnqueue{initiator: charge.InitiatorMerchant})
	if errors.Is(err, ErrDefaultPaymentMethodInvalid) || errors.Is(err, ErrDefaultPaymentMethodRequired) || errors.Is(err, charge.ErrAgreementRequired) {
		method = nil
		err = nil
	}
	if err != nil {
		return capacity, err
	}
	if method == nil {
		capacity.CreditLimit = 0
		capacity.BillingMode = BillingModePrepaid
	}
	return capacity, nil
}

// ValidateUsageBillingAccountTx validates the resulting settings, after a patch
// while the caller still owns the customer's transaction and spend lock.
func (s *MoneyService) ValidateUsageBillingAccountTx(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string) error {
	_, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil || cur == nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	row, err := q.GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	terms, err := s.resolveUsageTerms(ctx, q, row, cur)
	if err != nil {
		return err
	}
	if terms.creditLimit > 0 && terms.minimum > terms.creditLimit {
		return fmt.Errorf("minimum_invoice_amount cannot exceed the credit limit")
	}
	return nil
}

type usageTerms struct{ creditLimit, minimum, threshold int64 }

// Resolve the account override, merchant default and any tighter hard policy
// once. A collection threshold changes when to invoice, never how much is owed.
func (s *MoneyService) resolveUsageTerms(ctx context.Context, q *gen.Queries, row gen.BillingMoneySetting, cur *billing.UsageBillingCurrency) (usageTerms, error) {
	terms := usageTerms{creditLimit: usageCreditLimit(row, cur), minimum: usageMinimumInvoice(row, cur)}
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if err != nil {
		return terms, err
	}
	name := settings.Default
	if row.Tier != nil {
		if n, ok := settings.Tiers[*row.Tier]; ok {
			name = n
		}
	}
	customer, err := q.GetCustomer(ctx, gen.GetCustomerParams{MerchantID: row.MerchantID, ID: row.CustomerID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return terms, err
	}
	if err == nil && customer.BillingPolicy != nil {
		if _, ok := settings.Policies[*customer.BillingPolicy]; ok {
			name = *customer.BillingPolicy
		}
	}
	policy := settings.Policies[name]
	if policy.OutstandingCapAmount > 0 && policy.OutstandingCapAmount < terms.creditLimit {
		terms.creditLimit = policy.OutstandingCapAmount
	}
	terms.threshold = terms.creditLimit
	if v := settings.Config.CollectionThreshold; v != nil && *v > 0 && *v < terms.threshold {
		terms.threshold = *v
	}
	if v := policy.CollectionThresholdAmount; v != nil && *v > 0 {
		terms.threshold = min(*v, terms.creditLimit)
	}
	return terms, nil
}

// WakeUsageBillingTx rechecks a customer's changed settings in the same commit.
func (s *MoneyService) WakeUsageBillingTx(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string) error {
	return s.queueUsageBilling(ctx, q, customer, currency)
}

// usagePolicySettings resolves the configured policy, independently of whether
// a payment method currently makes that credit usable.
func (s *MoneyService) usagePolicySettings(ctx context.Context, row gen.BillingMoneySetting) (gen.BillingMoneySetting, error) {
	cfg, cur, err := s.usageBillingConfig(ctx, row.Currency)
	if err != nil || cfg == nil {
		return row, err
	}
	if cur == nil {
		row.CreditLimitAmount = 0
		row.BillingMode = BillingModePrepaid
		return row, nil
	}
	terms, err := s.resolveUsageTerms(ctx, s.db.Gen(ctx), row, cur)
	if err != nil {
		return row, err
	}
	row.CreditLimitAmount = terms.creditLimit
	row.BillingMode = BillingModeArrears
	if row.CreditLimitAmount == 0 {
		row.BillingMode = BillingModePrepaid
	}
	return row, nil
}
