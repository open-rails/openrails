package money

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/tenant"
)

// Custom credits (#475): tenant-defined consumable units (api-credits, gold-coins)
// with NO fixed FX, NEVER billed in. They reuse the money ledger primitives and
// the `currency` column, addressed by a QUALIFIED code `tenant-slug/name`.
// Unqualified codes (`usd`) remain built-in currencies (#474).

// ErrBillingUnitRequired is returned when a billing-layer path (invoice / owed /
// arrears / charge / auto-topup / account settings) is handed a qualified custom
// credit unit. Only built-in currencies may be billed in (#475 invariant).
var ErrBillingUnitRequired = errors.New("money: billing requires a built-in currency, not a custom credit unit")

// IsQualifiedUnit reports whether a unit code is a qualified custom-credit code
// (`tenant-slug/name`) rather than an unqualified built-in currency.
func IsQualifiedUnit(code string) bool {
	return strings.Contains(code, "/")
}

// splitQualified splits `slug/name`; both parts must be non-empty.
func splitQualified(code string) (slug, name string, ok bool) {
	i := strings.IndexByte(code, '/')
	if i <= 0 || i >= len(code)-1 {
		return "", "", false
	}
	slug, name = strings.TrimSpace(code[:i]), strings.TrimSpace(code[i+1:])
	return slug, name, slug != "" && name != ""
}

// RequireBillingCurrency rejects qualified custom-credit units at the billing
// boundary. Built-in currency codes (incl. "") pass through ValidateCurrency.
// NOTE (#475/#474 merge): a concurrent change (F) may introduce its own
// RequireBillingCurrency; reconcile to a single definition at merge.
func RequireBillingCurrency(code string) error {
	if IsQualifiedUnit(code) {
		return fmt.Errorf("%w: %q", ErrBillingUnitRequired, code)
	}
	return ValidateCurrency(code)
}

// ResolveUnit resolves a ledger unit code to its minor-unit decimals.
// Unqualified codes resolve against the built-in currency registry (#474).
// Qualified codes (`tenant-slug/name`) resolve against the ctx tenant's
// custom_credit_types: the slug must name the ctx tenant and the type must be
// active. Unknown / cross-tenant / inactive units are rejected.
func (s *MoneyService) ResolveUnit(ctx context.Context, code string) (decimals int, builtin bool, err error) {
	if !IsQualifiedUnit(code) {
		d, ok := CurrencyScale(code)
		if !ok {
			return 0, false, fmt.Errorf("money: unknown currency %q", code)
		}
		return d, true, nil
	}
	if s == nil || s.db == nil {
		return 0, false, fmt.Errorf("money service not initialized")
	}
	slug, name, ok := splitQualified(code)
	if !ok {
		return 0, false, fmt.Errorf("money: malformed custom credit code %q", code)
	}
	tid, err := tenant.Require(ctx)
	if err != nil {
		return 0, false, err
	}
	// The qualifying slug must name the ctx tenant — a custom unit is only valid
	// in its owning tenant's ledger (RLS scopes the lookup too).
	tr, err := s.db.Gen(ctx).GetTenantBySlug(ctx, slug)
	if err != nil {
		return 0, false, fmt.Errorf("money: unknown custom credit unit %q: %w", code, err)
	}
	if tr.ID != tid.UUID() {
		return 0, false, fmt.Errorf("money: custom credit unit %q not owned by ctx tenant", code)
	}
	ct, err := s.db.Gen(ctx).GetCustomCreditType(ctx, gen.GetCustomCreditTypeParams{TenantID: tid.UUID(), Name: name})
	if err != nil {
		return 0, false, fmt.Errorf("money: unknown custom credit unit %q: %w", code, err)
	}
	if !ct.Active {
		return 0, false, fmt.Errorf("money: custom credit unit %q is inactive", code)
	}
	return int(ct.Decimals), false, nil
}

// normalizeUnit normalizes a built-in currency (uppercase, "" → default) but
// leaves a qualified custom-credit code (`slug/name`) verbatim — uppercasing
// would corrupt it. Ledger consumption paths use this in place of
// normalizeCurrency so the `currency` column hosts both kinds (#475).
func normalizeUnit(code string) string {
	if IsQualifiedUnit(code) {
		return strings.TrimSpace(code)
	}
	return normalizeCurrency(code)
}

// validateUnit accepts either a built-in currency or a resolved, active custom
// credit unit. Ledger consumption paths (deposit/withdraw/hold/balance) call
// this instead of ValidateCurrency so they accept qualified custom units (#475).
func (s *MoneyService) validateUnit(ctx context.Context, code string) error {
	if IsQualifiedUnit(code) {
		_, _, err := s.ResolveUnit(ctx, code)
		return err
	}
	return ValidateCurrency(code)
}

// FormatAmount renders a stored integer minor-unit amount as a display string
// using the unit's decimals (built-in or custom). Pure given the resolved
// decimals; the API/admin boundary calls ResolveUnit first. e.g. 100 @ 2dp → "1.00".
func FormatAmount(minor int64, decimals int) string {
	if decimals <= 0 {
		return strconv.FormatInt(minor, 10)
	}
	neg := minor < 0
	if neg {
		minor = -minor
	}
	scale := int64(1)
	for i := 0; i < decimals; i++ {
		scale *= 10
	}
	whole, frac := minor/scale, minor%scale
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, decimals, frac)
}

// --- Registry CRUD (tenant-scoped; RLS enforced) ---

// DefineCustomCreditType upserts a custom credit unit for the ctx tenant
// (idempotent on name; reactivates + updates decimals).
func (s *MoneyService) DefineCustomCreditType(ctx context.Context, name string, decimals int) (gen.BillingCustomCreditType, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return gen.BillingCustomCreditType{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, "/ ") {
		return gen.BillingCustomCreditType{}, fmt.Errorf("money: invalid custom credit name %q", name)
	}
	if decimals < 0 || decimals > 18 {
		return gen.BillingCustomCreditType{}, fmt.Errorf("money: decimals out of range [0,18]: %d", decimals)
	}
	return s.db.Gen(ctx).DefineCustomCreditType(ctx, gen.DefineCustomCreditTypeParams{
		TenantID: tid.UUID(), Name: name, Decimals: int32(decimals),
	})
}

// ListCustomCreditTypes lists the ctx tenant's custom credit units.
func (s *MoneyService) ListCustomCreditTypes(ctx context.Context) ([]gen.BillingCustomCreditType, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListCustomCreditTypes(ctx, tid.UUID())
}

// SetCustomCreditTypeActive activates/deactivates a ctx-tenant custom credit unit.
func (s *MoneyService) SetCustomCreditTypeActive(ctx context.Context, name string, active bool) (gen.BillingCustomCreditType, error) {
	tid, err := tenant.Require(ctx)
	if err != nil {
		return gen.BillingCustomCreditType{}, err
	}
	return s.db.Gen(ctx).SetCustomCreditTypeActive(ctx, gen.SetCustomCreditTypeActiveParams{
		TenantID: tid.UUID(), Name: strings.TrimSpace(name), Active: active,
	})
}
