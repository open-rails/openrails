package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money/owed"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// KindUsageInvoices bills the due usage accounts and queues the due collections.
// It keeps the kind, and UsageInvoiceKey its key, that queued jobs carry.
const KindUsageInvoices = "openrails.usage_orders"

// UsageInvoicesInterval is the pass's cadence: how long a bill date or a due
// collection waits. A threshold crossing queues a pass at once.
const UsageInvoicesInterval = 5 * time.Minute

// usageOutstanding are a pass's states before it finishes.
var usageOutstanding = []rivertype.JobState{
	rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled,
}

// reopenedOwedSource is the line source of owed a chargeback reopened.
const reopenedOwedSource = owed.ReopenedSource

// UsageInvoiceArgs is one usage pass over every merchant: it bills the due
// accounts, then queues the due collections. Bill or Collect continues a pass
// past a full page.
type UsageInvoiceArgs struct {
	Bill    *UsageAccountKey `json:"bill,omitempty"`
	Collect *UsageInvoiceKey `json:"collect,omitempty"`
}

// UsageAccountKey is a due usage account's place in the pass.
type UsageAccountKey struct {
	DueAt      time.Time `json:"due_at"`
	MerchantID uuid.UUID `json:"merchant_id"`
	CustomerID uuid.UUID `json:"customer_id"`
	Currency   string    `json:"currency"`
}

// UsageInvoiceKey is a due collection's place in the pass.
type UsageInvoiceKey struct {
	DueAt      time.Time `json:"due_at"`
	MerchantID uuid.UUID `json:"merchant_id"`
	InvoiceID  uuid.UUID `json:"order_id"`
}

func (UsageInvoiceArgs) Kind() string { return KindUsageInvoices }

// InsertOpts coalesces passes, and a pass's continuations, while one is
// outstanding. A failed pass is not retried: what it left due, the next one
// takes.
func (UsageInvoiceArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "billing", MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: usageOutstanding}}
}

// usageBillingConfig is the merchant's usage_billing terms and its terms in
// currency: none without terms, or on a handle with no merchant
// configuration bound.
func (s *MoneyService) usageBillingConfig(ctx context.Context, currency string) (*billing.UsageBillingSettings, *billing.UsageBillingCurrency, error) {
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if errors.Is(err, merchantconfig.ErrNoSettings) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if cfg.UsageBilling == nil {
		return nil, nil, nil
	}
	for i := range cfg.UsageBilling.Currencies {
		if string(cfg.UsageBilling.Currencies[i].Currency) == currency {
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
	if err == nil && existing.UsageBillingAnchor != nil {
		anchor = *existing.UsageBillingAnchor
	}
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

// creditLimit is the arrears credit line a customer has in row's currency:
// its own credit limit when it has one, else its policy's cap in that
// currency, else the merchant's usage terms' (none without them).
func creditLimit(row gen.BillingMoneySetting, policy models.BillingPolicy, cfg *billing.UsageBillingCurrency) int64 {
	if row.CreditLimitOverride {
		return row.CreditLimitAmount
	}
	if policy.OutstandingCapAmount > 0 && inPolicyCurrency(policy, row.Currency) {
		return policy.OutstandingCapAmount
	}
	if cfg == nil {
		return 0
	}
	return cfg.CreditLimit
}

// inPolicyCurrency reports whether a policy's amounts bind money in currency:
// they are in its policy_currency, and one naming none is any money's, never
// a merchant's unit.
func inPolicyCurrency(policy models.BillingPolicy, currency string) bool {
	if policy.PolicyCurrency == "" {
		return !IsMerchantUnit(currency)
	}
	return strings.EqualFold(policy.PolicyCurrency, currency)
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

// enrollUsageBilling gives the customer's account in currency its bill date
// when the merchant's usage_billing terms cover the currency.
func (s *MoneyService) enrollUsageBilling(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string) error {
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil || cur == nil {
		return err
	}
	_, err = s.ensureUsageBillingAccount(ctx, q, customer, currency, cfg)
	return err
}

// queueUsageBilling runs inside the accrual transaction. The due record is the
// source of work; the coalesced River job only wakes its processor.
func (s *MoneyService) queueUsageBilling(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string) error {
	return s.requestUsageBilling(ctx, q, customer, currency, false, 0)
}

// RequestUsageCollection asks to collect an existing balance when a new request
// would exceed the credit limit. A denial commits this wakeup without reserving
// or charging the rejected work.
func (s *MoneyService) RequestUsageCollection(ctx context.Context, q *gen.Queries, customer identity.CustomerID, currency string) error {
	return s.requestUsageBilling(ctx, q, customer.UUID(), currency, true, 0)
}

func (s *MoneyService) requestUsageBilling(ctx context.Context, q *gen.Queries, customer uuid.UUID, currency string, force bool, pending int64) error {
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
	owed = addSat(owed, pending)
	if (!force && owed < terms.threshold) || owed <= 0 || owed < terms.minimum {
		return err
	}
	if err = q.FlagUsageCollectionDue(ctx, gen.FlagUsageCollectionDueParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, Now: s.now()}); err != nil {
		return err
	}
	_, err = s.db.InsertRiverJob(ctx, UsageInvoiceArgs{}, nil)
	return err
}

// MarkUsageAccountsDue marks due, after a change to the merchant's usage
// terms, each of its accounts whose arrears reach the resulting credit limit
// with enough unbilled to bill, or that has unbilled charges and no bill
// date.
func (s *MoneyService) MarkUsageAccountsDue(ctx context.Context) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	settings, err := merchantconfig.NewStore(s.db).Settings(ctx)
	if errors.Is(err, merchantconfig.ErrNoSettings) {
		return nil
	}
	if err != nil || settings.Config.UsageBilling == nil {
		return err
	}
	limits := map[string]int64{}
	minimums := map[string]int64{}
	for _, cur := range settings.Config.UsageBilling.Currencies {
		limits[string(cur.Currency)] = cur.CreditLimit
		minimums[string(cur.Currency)] = cur.MinimumInvoiceAmount
	}
	encodedLimits, err := json.Marshal(limits)
	if err != nil {
		return err
	}
	encodedMinimums, err := json.Marshal(minimums)
	if err != nil {
		return err
	}
	return s.db.Gen(ctx).MarkUsageAccountsDue(ctx, gen.MarkUsageAccountsDueParams{
		MerchantID: mid.UUID(), Limits: encodedLimits, Minimums: encodedMinimums, Policies: settings.PoliciesParam(), Now: s.now(),
	})
}

// BillDueUsageAccount checks one due usage account and bills what is due:
// everything pending at its bill date, the period so far once spending
// crossed its threshold or an admission asked to collect. The account is
// then due at its next bill date, or not at all with nothing pending. The
// check serializes with spending on the customer.
func (s *MoneyService) BillDueUsageAccount(ctx context.Context, customer uuid.UUID, currency string, now time.Time) (bool, error) {
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
		// What the usage and running charges accrued is owed before it is
		// billed, so the day before a bill date is in that bill whichever job
		// runs first.
		if _, err := s.WithDatabase(s.db.NewWithPgxTx(tx)).at(now).postEverything(ctx, q, mid.UUID(), customer, currency, usagePostingBilling); err != nil {
			return err
		}
		if _, err := postRunningChargesThrough(ctx, q, mid.UUID(), customer, currency, now, now); err != nil {
			return err
		}
		if cur == nil {
			// A currency without usage billing is not checked again until its
			// terms are; previously accepted payment operations still recover.
			// Owed a chargeback reopened needs no terms: it is billed.
			if reopened, err := q.HasPendingReopenedOwed(ctx, gen.HasPendingReopenedOwedParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency}); err != nil {
				return err
			} else if reopened {
				owed, err := s.moneyLedger(q, mid.UUID()).OutstandingOwed(ctx, customer, currency)
				if err != nil {
					return err
				}
				if billed, err = s.billPendingUsage(ctx, s.db.NewWithPgxTx(tx), customer, currency, owed, usageTerms{}, now); err != nil {
					return err
				}
			}
			return q.CompleteUsageBillingCheck(ctx, gen.CompleteUsageBillingCheckParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, Now: now})
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
		if due || row.UsageCollectionDue || owed >= terms.threshold {
			if billed, err = s.billPendingUsage(ctx, s.db.NewWithPgxTx(tx), customer, currency, owed, terms, now); err != nil {
				return err
			}
		}
		// Due next at the bill date while something is pending. The check
		// completes with the invoice: a failed one leaves the account due.
		var next *time.Time
		if _, err := q.EarliestPendingOwedAccrual(ctx, gen.EarliestPendingOwedAccrualParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency}); err == nil {
			at := *row.UsageNextBillAt
			if due {
				at = NextUsageBillingDate(now, *row.UsageBillingAnchor)
			}
			next = &at
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return q.CompleteUsageBillingCheck(ctx, gen.CompleteUsageBillingCheckParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, NextBillAt: next, Now: now})
	})
	return billed, err
}

// billPendingUsage bills the account's pending charges through now as one
// usage invoice, at most what no other invoice claims and not below the minimum.
func (s *MoneyService) billPendingUsage(ctx context.Context, d *db.DB, customer uuid.UUID, currency string, owed int64, terms usageTerms, now time.Time) (bool, error) {
	q := d.Gen(ctx)
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	from, err := q.EarliestPendingOwedAccrual(ctx, gen.EarliestPendingOwedAccrualParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Each usage invoice ends after the last: a close never ends where one did.
	cutoff := now.Add(time.Microsecond)
	last, err := q.LatestUsageInvoiceEndInCurrency(ctx, gen.LatestUsageInvoiceEndInCurrencyParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
	if err != nil {
		return false, err
	}
	if !last.Before(cutoff) {
		cutoff = last.Add(time.Microsecond)
	}
	sources, err := q.SumPendingOwedItemAmountBySourceInPeriod(ctx, gen.SumPendingOwedItemAmountBySourceInPeriodParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency, PeriodStartsAt: from, PeriodEndsAt: cutoff})
	if err != nil {
		return false, err
	}
	var pending int64
	reopened := false
	for _, source := range sources {
		pending += source.Amount
		// Owed a chargeback reopened is billed at once, whatever the minimum.
		reopened = reopened || source.Source == reopenedOwedSource && source.Amount > 0
	}
	claimed, err := q.SumOwedInvoiceClaims(ctx, gen.SumOwedInvoiceClaimsParams{MerchantID: mid.UUID(), CustomerID: customer, Currency: currency})
	if err != nil {
		return false, err
	}
	amount := min(pending, max(owed-claimed, 0))
	if amount <= 0 || amount < terms.minimum && !reopened {
		return false, nil
	}
	invoice, err := s.finalizeUsageTx(ctx, d, identity.CustomerID(customer), currency, from, cutoff, now)
	if err != nil {
		return false, fmt.Errorf("invoice usage: %w", err)
	}
	return invoice != nil, nil
}

// canCollectUsage reports whether usage can be collected from row's customer:
// collection is not revoked and its default card is chargeable under a live
// unscheduled mandate.
func (s *MoneyService) canCollectUsage(ctx context.Context, q *gen.Queries, row gen.BillingMoneySetting) (bool, error) {
	if row.CollectionRevokedAt != nil || row.DefaultPaymentMethodID == nil {
		return false, nil
	}
	method, err := s.collectionMethodFor(ctx, q, row.MerchantID, row.CustomerID, row.Currency, usageCollectionEnqueue{
		paymentMethodID: row.DefaultPaymentMethodID, initiator: charge.InitiatorMerchant, read: true,
	})
	if errors.Is(err, ErrDefaultPaymentMethodInvalid) || errors.Is(err, ErrDefaultPaymentMethodRequired) || errors.Is(err, charge.ErrAgreementRequired) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return method != nil, nil
}

// usageAdmissionBillingMode is the billing mode usageAdmissionCapacity admits
// row's customer under: under usage terms arrears whenever usage can be
// collected, even on a credit line of 0; revoked collection is prepaid.
func (s *MoneyService) usageAdmissionBillingMode(ctx context.Context, row gen.BillingMoneySetting) (string, error) {
	if row.CollectionRevokedAt != nil {
		return BillingModePrepaid, nil
	}
	cfg, cur, err := s.usageBillingConfig(ctx, row.Currency)
	if err != nil {
		return "", err
	}
	if cfg == nil {
		return row.BillingMode, nil
	}
	if cur == nil {
		return BillingModePrepaid, nil
	}
	collectible, err := s.canCollectUsage(ctx, s.db.Gen(ctx), row)
	if errors.Is(err, charge.ErrNoRoute) {
		return BillingModePrepaid, nil
	}
	if err != nil {
		return "", err
	}
	if collectible {
		return BillingModeArrears, nil
	}
	return BillingModePrepaid, nil
}

// GetAdmissionBillingMode returns the billing mode the spend gate admits under.
func (s *MoneyService) GetAdmissionBillingMode(ctx context.Context, payer identity.CustomerID, currency string) (string, error) {
	cur := normalizeCurrency(currency)
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	if IsMerchantUnit(cur) {
		return BillingModePrepaid, nil
	}
	if err := RequireBillingCurrency(cur); err != nil {
		return "", err
	}
	row, err := s.db.Gen(ctx).GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
		MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: cur,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		row = gen.BillingMoneySetting{MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: cur, BillingMode: BillingModePrepaid}
	} else if err != nil {
		return "", err
	}
	return s.usageAdmissionBillingMode(ctx, row)
}

// usageAdmissionCapacity resolves capacity's credit line (creditLimit) and,
// under the merchant's usage_billing terms, its billing mode: arrears while
// the customer can be collected from, else prepaid. Without terms the
// customer's own mode stands, and a prepaid customer's line is unread.
func (s *MoneyService) usageAdmissionCapacity(ctx context.Context, q *gen.Queries, payer identity.CustomerID, currency string, capacity AdmissionCapacity) (AdmissionCapacity, error) {
	cfg, cur, err := s.usageBillingConfig(ctx, currency)
	if err != nil {
		return capacity, err
	}
	if cfg != nil && cur == nil {
		capacity.BillingMode = BillingModePrepaid
		capacity.CreditLimit = 0
		return capacity, nil
	}
	if cfg == nil && capacity.BillingMode != BillingModeArrears {
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
	capacity.CreditLimit = terms.creditLimit
	if cfg == nil {
		return capacity, nil
	}
	if terms.creditLimit > 0 && terms.minimum > terms.creditLimit {
		return capacity, fmt.Errorf("usage minimum invoice exceeds the credit limit for %s", currency)
	}
	capacity.BillingMode = BillingModeArrears
	collectible, err := s.canCollectUsage(ctx, q, row)
	if err != nil {
		return capacity, err
	}
	if !collectible {
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

// resolveUsageTerms resolves the customer's credit line (creditLimit) under
// its bound policy, its minimum invoice and its collection threshold once. A
// collection threshold changes when to invoice, never how much is owed. cur
// is nil without usage terms.
func (s *MoneyService) resolveUsageTerms(ctx context.Context, q *gen.Queries, row gen.BillingMoneySetting, cur *billing.UsageBillingCurrency) (usageTerms, error) {
	terms := usageTerms{minimum: usageMinimumInvoice(row, cur)}
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
	terms.creditLimit = creditLimit(row, policy, cur)
	terms.threshold = terms.creditLimit
	if cur != nil && cur.CollectionThreshold != nil && *cur.CollectionThreshold > 0 && *cur.CollectionThreshold < terms.threshold {
		terms.threshold = *cur.CollectionThreshold
	}
	if v := policy.CollectionThresholdAmount; inPolicyCurrency(policy, row.Currency) && v != nil && *v > 0 {
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
	if err != nil {
		return row, err
	}
	if cfg != nil && cur == nil {
		row.CreditLimitAmount = 0
		row.BillingMode = BillingModePrepaid
		return row, nil
	}
	if cfg == nil && row.BillingMode != BillingModeArrears {
		return row, nil
	}
	terms, err := s.resolveUsageTerms(ctx, s.db.Gen(ctx), row, cur)
	if err != nil {
		return row, err
	}
	row.CreditLimitAmount = terms.creditLimit
	if cfg == nil {
		return row, nil
	}
	row.BillingMode = BillingModeArrears
	if row.CreditLimitAmount == 0 {
		row.BillingMode = BillingModePrepaid
	}
	return row, nil
}
