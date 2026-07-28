package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Custom credits (#475): merchant-defined consumable units (api-credits, gold-coins)
// with NO fixed FX, NEVER billed in. They reuse the money ledger primitives and
// the `currency` column, addressed by a QUALIFIED code `merchant-slug/name`.
// Unqualified codes (`usd`) remain built-in currencies (#474).

// ErrBillingUnitRequired is returned when a billing-layer path (invoice / owed /
// arrears / charge / auto-topup / account settings) is handed a qualified custom
// credit unit. Only built-in currencies may be billed in (#475 invariant).
var ErrBillingUnitRequired = errors.New("money: billing requires a built-in currency, not a custom credit unit")

// IsQualifiedUnit reports whether a unit code is a qualified custom-credit code
// (`merchant-slug/name`) rather than an unqualified built-in currency.
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

// RequireBillingCurrency lives in currency.go alongside the registry; it rejects
// qualified custom-credit units (#475) at the billing boundary.

// ResolveUnit resolves a ledger unit code to its minor-unit decimals.
// Unqualified codes resolve against the built-in currency registry (#474).
// Qualified codes (`merchant-slug/name`) resolve against the ctx merchant's
// custom_credit_types: the slug must name the ctx merchant and the type must be
// active. Unknown / cross-merchant / inactive units are rejected.
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
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, false, err
	}
	// The qualifying slug must name the ctx merchant — a custom unit is only valid
	// in its owning merchant's ledger (RLS scopes the lookup too).
	tr, err := s.db.Gen(ctx).GetMerchantBySlug(ctx, slug)
	if err != nil {
		return 0, false, fmt.Errorf("money: unknown custom credit unit %q: %w", code, err)
	}
	if tr.ID != tid.UUID() {
		return 0, false, fmt.Errorf("money: custom credit unit %q not owned by ctx merchant", code)
	}
	ct, err := s.db.Gen(ctx).GetCustomCreditType(ctx, gen.GetCustomCreditTypeParams{MerchantID: tid.UUID(), Name: name})
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

// Registry writes live in the catalog sidecar push (#706): custom_credit_types
// rows are auto-defined from catalog_credit_balances units. There is no admin
// CRUD surface here.
