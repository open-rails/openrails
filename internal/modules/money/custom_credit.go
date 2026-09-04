package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Custom credits (#475): merchant-defined consumable units (api-credits, gold-coins)
// with NO fixed FX, NEVER billed in. They reuse the money ledger primitives and
// the `currency` column, addressed internally by credit:<registry UUID>.
// External merchant-slug/name values resolve once at the service boundary.
// Unqualified codes (`usd`) remain built-in currencies (#474).

// ErrBillingUnitRequired is returned when a billing-layer path (invoice / owed /
// arrears / charge / auto-topup / account settings) is handed a qualified custom
// credit unit. Only built-in currencies may be billed in (#475 invariant).
var ErrBillingUnitRequired = errors.New("money: billing requires a built-in currency, not a custom credit unit")

// IsQualifiedUnit reports whether a unit code is a qualified custom-credit code
// (public `merchant-slug/name` or internal `credit:UUID`).
func IsQualifiedUnit(code string) bool {
	return strings.Contains(code, "/") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(code)), "credit:")
}

// CreditUnitCode is the immutable spelling stored in ledger, grant and hold keys.
func CreditUnitCode(id uuid.UUID) string { return "credit:" + id.String() }

func creditUnitID(code string) (uuid.UUID, error) {
	if !strings.HasPrefix(code, "credit:") {
		return uuid.Nil, fmt.Errorf("custom credit unit must use its immutable registry identity")
	}
	id, err := uuid.Parse(strings.TrimPrefix(code, "credit:"))
	if err != nil || id == uuid.Nil || CreditUnitCode(id) != code {
		return uuid.Nil, fmt.Errorf("invalid custom credit identity %q", code)
	}
	return id, nil
}

// CustomUnitCode resolves a name inside the already authorized merchant.
func (s *MoneyService) CustomUnitCode(ctx context.Context, name string) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	row, err := s.db.Gen(ctx).GetCustomCreditType(ctx, gen.GetCustomCreditTypeParams{MerchantID: mid.UUID(), Name: name})
	if err != nil {
		return "", err
	}
	if !row.Active {
		return "", fmt.Errorf("custom credit unit is inactive")
	}
	return CreditUnitCode(row.ID), nil
}

// CustomUnitName projects the stable registry name for an owned unit UUID.
func (s *MoneyService) CustomUnitName(ctx context.Context, code string) (string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	id, err := creditUnitID(code)
	if err != nil {
		return "", err
	}
	row, err := s.db.Gen(ctx).GetCustomCreditTypeByID(ctx, gen.GetCustomCreditTypeByIDParams{MerchantID: mid.UUID(), ID: id})
	if err != nil {
		return "", err
	}
	return row.Name, nil
}

// RequireBillingCurrency lives in currency.go alongside the registry; it rejects
// qualified custom-credit units (#475) at the billing boundary.

// ResolveUnit resolves a ledger unit code to its minor-unit decimals.
// Unqualified codes resolve against the built-in currency registry (#474).
// Custom UUIDs resolve against the ctx merchant's existing registry.
// Human-readable names never reach this stored-unit path.
func (s *MoneyService) ResolveUnit(ctx context.Context, code string) (decimals int, builtin bool, err error) {
	if !IsQualifiedUnit(code) {
		d, ok := moneyutil.CurrencyScale(code)
		if !ok {
			return 0, false, fmt.Errorf("money: unknown currency %q", code)
		}
		return d, true, nil
	}
	if s == nil || s.db == nil {
		return 0, false, fmt.Errorf("money service not initialized")
	}
	id, err := creditUnitID(code)
	if err != nil {
		return 0, false, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return 0, false, err
	}
	ct, err := s.db.Gen(ctx).GetCustomCreditTypeByID(ctx, gen.GetCustomCreditTypeByIDParams{MerchantID: tid.UUID(), ID: id})
	if err != nil {
		return 0, false, fmt.Errorf("unknown custom credit identity for merchant: %w", err)
	}
	if !ct.Active {
		return 0, false, fmt.Errorf("money: custom credit unit %q is inactive", code)
	}
	return int(ct.Decimals), false, nil
}

// normalizeUnit preserves canonical custom UUID spelling and uppercases ISO codes.
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
	return moneyutil.ValidateCurrency(code)
}

// Registry writes live in the catalog sidecar push (#706): custom_credit_types
// rows are auto-defined from catalog_credit_balances units. There is no admin
// CRUD surface here.
