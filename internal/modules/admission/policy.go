// Package admission implements OpenRails service admission for payer money
// capacity, delegated spend windows, and delegated wasted-spend cutoffs.
package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/budgets"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// BillingPolicyStore is the or#897 policy registry: named policies plus the
// bindings that say which one applies to whom. It REPLACES payer_spend_limits,
// which was the same machine with one implicit meaning — a window cap — and no
// way to say that a different quantity is the one being capped.
type BillingPolicyStore struct {
	db *db.DB
}

func NewBillingPolicyStore(database *db.DB) *BillingPolicyStore {
	return &BillingPolicyStore{db: database}
}

// ResolvedPolicy is the effective policy for one (payer, tier) plus the name it
// resolved through, so a denial can say WHICH policy refused.
//
// A zero ResolvedPolicy (no binding at all) is the merchant that has declared
// nothing: Kind is empty and the admission path falls back to the payer's own
// arrears credit limit under outstanding-cap semantics — the conservative
// reading, since it is the one that can refuse.
type ResolvedPolicy struct {
	Name string
	Kind models.BillingPolicyKind
	// OutstandingCapAmount is the declared credit line on unpaid arrears
	// (kind=outstanding_cap). Zero defers to the payer's own arrears limit.
	OutstandingCapAmount int64
	SpendWindows         []budgets.BudgetWindow
	PolicyCurrency       string
	// BadSpendWindows are the #497 per-PAYER wasted-spend grace windows;
	// direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
}

// GatesOnOutstandingOwed reports whether unpaid arrears reduce this payer's
// admission headroom. This ONE branch is the whole difference between the two
// seed businesses: the API business's $200 line is a ceiling on DEBT, while the
// cloud business's $2k/month cap is a ceiling on NEW SPEND and lets prior debt
// drive delinquency instead.
func (p ResolvedPolicy) GatesOnOutstandingOwed() bool {
	return p.Kind != models.BillingPolicyWindowSpendCap
}

type catalogUsageLimitWindow struct {
	Window string `json:"window"`
	Amount int64  `json:"amount"`
}

// UpsertPolicy declares (or redeclares) one named policy. The body must already
// have passed merchantconfig.NormalizeBillingPolicy — both transports call it.
func (s *BillingPolicyStore) UpsertPolicy(ctx context.Context, name string, policy models.BillingPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("admission: encode billing policy: %w", err)
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return s.db.Gen(ctx).UpsertBillingPolicy(ctx, gen.UpsertBillingPolicyParams{
			ID:         uuidutil.NewV7(),
			MerchantID: tid.UUID(),
			Name:       name,
			Policy:     policyJSON,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	})
}

// ListPolicies returns every declared policy, keyed by name.
func (s *BillingPolicyStore) ListPolicies(ctx context.Context) (map[string]models.BillingPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]models.BillingPolicy{}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListBillingPolicies(ctx, tid.UUID())
		if err != nil {
			return err
		}
		for _, r := range rows {
			var body models.BillingPolicy
			if len(r.Policy) > 0 {
				if uerr := json.Unmarshal(r.Policy, &body); uerr != nil {
					return fmt.Errorf("admission: decode billing policy %q: %w", r.Name, uerr)
				}
			}
			out[r.Name] = body
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BindPolicy points one rung at a policy name. A non-zero payer binds the
// per-customer rung; a non-empty tier binds the per-tier rung; neither binds the
// merchant default. Binding moves no money — it is the merchant's runtime lever.
func (s *BillingPolicyStore) BindPolicy(ctx context.Context, payer identity.CustomerID, tier, policyName string) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	tier = strings.TrimSpace(tier)
	if !payer.IsZero() && tier != "" {
		return fmt.Errorf("admission: bind to a customer OR a tier, not both")
	}
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		switch {
		case !payer.IsZero():
			// Materialize the payable customers row so the binding FK is satisfied
			// on first write (same reason the retired table needed it, #317).
			if _, err := db.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
				return err
			}
			subjectID := payer.UUID()
			return q.UpsertBillingPolicyBindingCustomer(ctx, gen.UpsertBillingPolicyBindingCustomerParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID, CustomerID: &subjectID,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		case tier != "":
			return q.UpsertBillingPolicyBindingTier(ctx, gen.UpsertBillingPolicyBindingTierParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID, Tier: &tier,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		default:
			return q.UpsertBillingPolicyBindingDefault(ctx, gen.UpsertBillingPolicyBindingDefaultParams{
				ID: uuidutil.NewV7(), MerchantID: tenantID,
				PolicyName: policyName, CreatedAt: now, UpdatedAt: now,
			})
		}
	})
}

// ListDeclarativeBindings returns the DECLARATIVE rungs — the merchant default
// and the per-tier bindings. Per-customer bindings are excluded on purpose: they
// are runtime segmentation state (one row per bound customer), so listing them
// would scale with records on file rather than with configuration.
func (s *BillingPolicyStore) ListDeclarativeBindings(ctx context.Context) ([]models.BillingPolicyBinding, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out []models.BillingPolicyBinding
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListDeclarativeBillingPolicyBindings(ctx, tid.UUID())
		if err != nil {
			return err
		}
		for _, r := range rows {
			b := models.BillingPolicyBinding{
				ID: r.ID, MerchantID: r.MerchantID, CustomerID: r.CustomerID,
				PolicyName: r.PolicyName, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			}
			if r.Tier != nil {
				b.Tier = *r.Tier
			}
			out = append(out, b)
		}
		return nil
	})
	return out, err
}

// Resolve returns the effective policy for (payer, tier). No binding yields a
// zero ResolvedPolicy, which the admission path reads as outstanding-cap
// semantics over the payer's own arrears credit limit.
func (s *BillingPolicyStore) Resolve(ctx context.Context, payer identity.CustomerID, tier string) (ResolvedPolicy, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return ResolvedPolicy{}, err
	}
	tenantID := tid.UUID()
	subjectID := payer.UUID()
	var out ResolvedPolicy
	// The predicates are `= $n OR IS NULL`, so one read considers all three rungs
	// and the ORDER BY picks the most specific.
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).ResolveBillingPolicy(ctx, gen.ResolveBillingPolicyParams{
			MerchantID: tenantID, CustomerID: &subjectID, Tier: &tier,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		var body models.BillingPolicy
		if len(row.Policy) > 0 {
			if uerr := json.Unmarshal(row.Policy, &body); uerr != nil {
				return fmt.Errorf("admission: decode billing policy %q: %w", row.PolicyName, uerr)
			}
		}
		out = ResolvedPolicy{
			Name:                 row.PolicyName,
			Kind:                 body.Kind,
			OutstandingCapAmount: body.OutstandingCapAmount,
			SpendWindows:         toBudgetWindows(body.SpendWindows),
			PolicyCurrency:       body.PolicyCurrency,
			BadSpendWindows:      body.BadSpendWindows,
		}
		return nil
	})
	if err != nil {
		return ResolvedPolicy{}, err
	}
	return out, nil
}

func (s *BillingPolicyStore) GetProductUsageLimitWindows(ctx context.Context, payer identity.CustomerID, measure string) ([]budgets.BudgetWindow, error) {
	measure = strings.TrimSpace(measure)
	if measure == "" {
		return nil, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	var out []budgets.BudgetWindow
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Qx(ctx).Query(ctx, `
SELECT usage_limit_key, windows
FROM openrails.product_usage_limit_bindings
WHERE merchant_id = $1
  AND customer_id = $2
  AND measure = $3
  AND revoked_at IS NULL
  AND starts_at <= now()
  AND (ends_at IS NULL OR ends_at > now())
ORDER BY usage_limit_key`, tenantID, payer.UUID(), measure)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var raw []byte
			if err := rows.Scan(&key, &raw); err != nil {
				return err
			}
			var windows []catalogUsageLimitWindow
			if err := json.Unmarshal(raw, &windows); err != nil {
				return fmt.Errorf("admission: decode product usage-limit windows %q: %w", key, err)
			}
			for _, window := range windows {
				seconds, err := usageLimitWindowSeconds(window.Window)
				if err != nil {
					return fmt.Errorf("admission: product usage-limit %q: %w", key, err)
				}
				if window.Amount < 0 {
					continue
				}
				out = append(out, budgets.BudgetWindow{
					Key:           key + ":" + strings.TrimSpace(window.Window),
					WindowSeconds: seconds,
					Limit:         window.Amount,
				})
			}
		}
		return rows.Err()
	})
	return out, err
}

func usageLimitWindowSeconds(value string) (int64, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, fmt.Errorf("window is required")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value[:len(value)-1]), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("window %q must be a positive whole h or d value", value)
	}
	switch value[len(value)-1] {
	case 'h':
		return int64((time.Duration(n) * time.Hour) / time.Second), nil
	case 'd':
		return int64((time.Duration(n) * 24 * time.Hour) / time.Second), nil
	default:
		return 0, fmt.Errorf("window %q must use h or d", value)
	}
}

// InvokerSpendLimitStore reads/writes per-invoker spend limits (#473/#517) —
// {scope, scope_key, windows[]} rows in openrails.invoker_spend_limits. These are
// the payer's own caps on its delegated invokers/roles (payer-set only). The admit
// path (LoadAll) reads every scope to compose the verdict.
type InvokerSpendLimitStore struct {
	db *db.DB
}

func NewInvokerSpendLimitStore(database *db.DB) *InvokerSpendLimitStore {
	return &InvokerSpendLimitStore{db: database}
}

// InvokerSpendLimit is one stored limit: {scope, scopeKey, windows[]}. scopeKey is
// the immutable role uuid (scope=role) / invoker string (scope=invoker) / tier key
// (scope=invoker_tier).
type InvokerSpendLimit struct {
	Scope    string
	ScopeKey string
	Windows  []models.BudgetWindowPolicy
}

// ValidateInvokerSpendLimit validates and canonicalizes one payer-owned
// delegated-spend policy before either transport writes it.
func ValidateInvokerSpendLimit(p InvokerSpendLimit) (InvokerSpendLimit, error) {
	p.Scope = budgets.NormalizeScope(p.Scope)
	switch p.Scope {
	case budgets.ScopeInvoker, budgets.ScopeRole, budgets.ScopeInvokerTrustLevel:
	default:
		return InvokerSpendLimit{}, fmt.Errorf("scope must be %q, %q, or %q", budgets.ScopeInvoker, budgets.ScopeRole, budgets.ScopeInvokerTrustLevel)
	}
	p.ScopeKey = strings.TrimSpace(p.ScopeKey)
	if p.ScopeKey == "" {
		return InvokerSpendLimit{}, fmt.Errorf("scope_key required")
	}
	if len(p.Windows) == 0 {
		return InvokerSpendLimit{}, fmt.Errorf("windows required")
	}
	for i := range p.Windows {
		window := &p.Windows[i]
		window.Key = strings.TrimSpace(window.Key)
		if window.Key == "" {
			return InvokerSpendLimit{}, fmt.Errorf("windows[%d].key required", i)
		}
		if window.WindowSeconds <= 0 {
			return InvokerSpendLimit{}, fmt.Errorf("windows[%d].window_seconds must be positive", i)
		}
		if window.Limit < 0 {
			return InvokerSpendLimit{}, fmt.Errorf("windows[%d].limit must be non-negative", i)
		}
		window.Currency = money.NormalizeCurrency(window.Currency)
		if window.Currency != "" {
			if err := moneyutil.ValidateCurrency(window.Currency); err != nil {
				return InvokerSpendLimit{}, fmt.Errorf("windows[%d].currency invalid: %w", i, err)
			}
		}
	}
	return p, nil
}

func invokerSpendLimitFromGen(r gen.OpenrailsInvokerSpendLimit) (InvokerSpendLimit, error) {
	p := InvokerSpendLimit{Scope: r.Scope, ScopeKey: r.ScopeKey}
	if len(r.Windows) > 0 {
		if err := json.Unmarshal(r.Windows, &p.Windows); err != nil {
			return InvokerSpendLimit{}, fmt.Errorf("admission: decode invoker spend-limit windows: %w", err)
		}
	}
	return p, nil
}

// LoadAll returns every invoker spend limit for a payer (the admit path composes
// all of them). Returns nil when none exist.
func (s *InvokerSpendLimitStore) LoadAll(ctx context.Context, payer identity.CustomerID) ([]InvokerSpendLimit, error) {
	tid, terr := merchant.Require(ctx) // #336/#474: no default merchant
	if terr != nil {
		return nil, terr
	}
	var out []InvokerSpendLimit
	err := s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var err error
		out, err = s.loadAll(ctx, tid.UUID(), payer)
		return err
	})
	return out, err
}

func (s *InvokerSpendLimitStore) loadAll(ctx context.Context, tenantID uuid.UUID, payer identity.CustomerID) ([]InvokerSpendLimit, error) {
	var out []InvokerSpendLimit
	rows, err := s.db.Gen(ctx).ListInvokerSpendLimits(ctx, gen.ListInvokerSpendLimitsParams{
		MerchantID: tenantID, CustomerID: payer.UUID(),
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p, err := invokerSpendLimitFromGen(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// withPayerWriteTx serializes every spend-delegation write for one
// merchant+payer and keeps the lock for the full transaction. Replace therefore
// cannot interleave its read/delete/upsert sequence with a singular upsert.
func (s *InvokerSpendLimitStore) withPayerWriteTx(ctx context.Context, payer identity.CustomerID, fn func(context.Context, *InvokerSpendLimitStore, uuid.UUID) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("admission: invoker spend-limit store not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	lockKey := tid.String() + ":" + payer.UUID().String()
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
			return fmt.Errorf("admission: lock invoker spend limits: %w", err)
		}
		txdb := s.db.NewWithPgxTx(tx)
		if _, err := db.EnsureCustomerID(ctx, txdb.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		return fn(ctx, NewInvokerSpendLimitStore(txdb), tenantID)
	})
}

// Upsert writes one invoker spend limit (payer-set) under the same serialized
// transaction boundary used by Replace.
func (s *InvokerSpendLimitStore) Upsert(ctx context.Context, payer identity.CustomerID, p InvokerSpendLimit) error {
	return s.withPayerWriteTx(ctx, payer, func(ctx context.Context, txStore *InvokerSpendLimitStore, tenantID uuid.UUID) error {
		return txStore.upsert(ctx, tenantID, payer, p)
	})
}

func (s *InvokerSpendLimitStore) upsert(ctx context.Context, tenantID uuid.UUID, payer identity.CustomerID, p InvokerSpendLimit) error {
	now := time.Now().UTC()
	windowsJSON, err := json.Marshal(p.Windows)
	if err != nil {
		return fmt.Errorf("admission: encode invoker spend-limit windows: %w", err)
	}
	return s.db.Gen(ctx).UpsertInvokerSpendLimit(ctx, gen.UpsertInvokerSpendLimitParams{
		ID:            uuidutil.NewV7(),
		MerchantID:    tenantID,
		CustomerID:    payer.UUID(),
		Scope:         budgets.NormalizeScope(p.Scope), // #491: store canonical invoker
		ScopeKey:      p.ScopeKey,
		Windows:       windowsJSON,
		PolicyVersion: 1,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
}

// Replace atomically replaces the complete payer-owned policy document. Any
// failed delete/upsert rolls the transaction back to the prior document.
func (s *InvokerSpendLimitStore) Replace(ctx context.Context, payer identity.CustomerID, next []InvokerSpendLimit) error {
	return s.withPayerWriteTx(ctx, payer, func(ctx context.Context, txStore *InvokerSpendLimitStore, tenantID uuid.UUID) error {
		if _, err := txStore.db.Gen(ctx).DeleteAllInvokerSpendLimits(ctx, gen.DeleteAllInvokerSpendLimitsParams{
			MerchantID: tenantID,
			CustomerID: payer.UUID(),
		}); err != nil {
			return err
		}
		for _, row := range next {
			if err := txStore.upsert(ctx, tenantID, payer, row); err != nil {
				return err
			}
		}
		return nil
	})
}

func toBudgetWindows(ws []models.BudgetWindowPolicy) []budgets.BudgetWindow {
	out := make([]budgets.BudgetWindow, 0, len(ws))
	for _, w := range ws {
		out = append(out, budgets.BudgetWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit, Currency: w.Currency})
	}
	return out
}
