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

// PayerSpendLimitStore loads + stores per-payer, per-trust-level admission
// policies (openrails.payer_spend_limits). Account money state stays in
// money_accounts.
type PayerSpendLimitStore struct {
	db *db.DB
}

func NewPayerSpendLimitStore(database *db.DB) *PayerSpendLimitStore {
	return &PayerSpendLimitStore{db: database}
}

// PayerSpendLimits is a trust level's enforceable money policy.
type PayerSpendLimits struct {
	BudgetWindows  []budgets.BudgetWindow
	PolicyCurrency string
	// BadSpendWindows are the #497 per-PAYER direct-credential wasted-spend grace
	// windows for this trust level; direct-payer overage is charged at report time.
	BadSpendWindows []models.BudgetWindowPolicy
}

type catalogUsageLimitWindow struct {
	Window string `json:"window"`
	Amount int64  `json:"amount"`
}

// UpsertPayerSpendLimitsFull sets the full trust-level money policy.
// A ZERO payer writes the TENANT-WIDE DEFAULT policy for the trust level (#477):
// the platform capacity ladder declared once, applied to every payer at that
// trust level (selected by GetPayerSpendLimits when the payer has no own override).
func (s *PayerSpendLimitStore) UpsertPayerSpendLimitsFull(ctx context.Context, payer identity.CustomerID, trustLevel string, policy models.TrustLevelMoneyPolicy) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	tenantID := tid.UUID()
	now := time.Now().UTC()
	return s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		policyJSON, err := json.Marshal(policy)
		if err != nil {
			return fmt.Errorf("admission: encode trust-level policy: %w", err)
		}
		// Merchant-wide default (#477): no subject row to materialize, NULL subject.
		if payer.IsZero() {
			return s.db.Gen(ctx).UpsertPayerSpendLimitDefault(ctx, gen.UpsertPayerSpendLimitDefaultParams{
				ID:            uuidutil.NewV7(),
				MerchantID:    tenantID,
				Tier:          trustLevel,
				Policy:        policyJSON,
				PolicyVersion: 1,
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		}
		// Per-subject override: materialize the payable customers row so the
		// payer_spend_limits FK (migration 076) is satisfied on first write (#317).
		if _, err := db.EnsureCustomerID(ctx, s.db.Qx(ctx), tenantID, payer.UUID().String()); err != nil {
			return err
		}
		subjectID := payer.UUID()
		return s.db.Gen(ctx).UpsertPayerSpendLimit(ctx, gen.UpsertPayerSpendLimitParams{
			ID:            uuidutil.NewV7(),
			MerchantID:    tenantID,
			CustomerID:    &subjectID,
			Tier:          trustLevel,
			Policy:        policyJSON,
			PolicyVersion: 1,
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
}

// GetPayerSpendLimits returns the enforceable money policy for (payer, trustLevel).
// A missing row yields an empty policy.
func (s *PayerSpendLimitStore) GetPayerSpendLimits(ctx context.Context, payer identity.CustomerID, trustLevel string) (PayerSpendLimits, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return PayerSpendLimits{}, err
	}
	tenantID := tid.UUID()
	row := new(models.TrustLevelPolicy)
	found := false
	// GetPayerSpendLimits resolves the payer's own override else the merchant-wide default
	// (NULL subject, #477). The query's subject predicate is `= $2 OR IS NULL`, so
	// passing the payer uuid matches both the override and the default.
	subjectID := payer.UUID()
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		genRow, e := s.db.Gen(ctx).GetPayerSpendLimits(ctx, gen.GetPayerSpendLimitsParams{
			MerchantID: tenantID, CustomerID: &subjectID, Tier: trustLevel,
		})
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if len(genRow.Policy) > 0 {
			if uerr := json.Unmarshal(genRow.Policy, &row.Policy); uerr != nil {
				return fmt.Errorf("admission: decode trust-level policy: %w", uerr)
			}
		}
		found = true
		return nil
	})
	if err != nil {
		return PayerSpendLimits{}, err
	}
	if !found {
		return PayerSpendLimits{}, nil
	}
	return PayerSpendLimits{
		BudgetWindows:   toBudgetWindows(row.Policy.BudgetWindows),
		PolicyCurrency:  row.Policy.PolicyCurrency,
		BadSpendWindows: row.Policy.BadSpendWindows,
	}, nil
}

func (s *PayerSpendLimitStore) GetProductUsageLimitWindows(ctx context.Context, payer identity.CustomerID, measure string) ([]budgets.BudgetWindow, error) {
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
