package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Billing modes.
const (
	BillingModePrepaid = "prepaid"
	BillingModeArrears = "arrears"
)

// SpendDecision is the outcome of an account-capacity authorization
// (AuthorizeAndHold): whether the estimated charge is permitted and, if denied,
// the deny code. HardStop distinguishes a hard block from a warn-only breach.
type SpendDecision struct {
	Allowed  bool   `json:"allowed"`
	HardStop bool   `json:"hard_stop"`
	DenyCode string `json:"deny_code,omitempty"`
}

// DefaultAccountSettings returns the implicit policy for an payer that has no
// explicit settings row: prepaid, 365-day default expiry. Used by
// GetAccountSettings and the enforcement path so an unconfigured account "just
// works" (prepaid, balance-gated only).
func DefaultAccountSettings(payer identity.CustomerID) *models.MoneyAccount {
	hours := 365 * 24
	return &models.MoneyAccount{
		CustomerID:               payer.UUID(),
		BillingMode:              BillingModePrepaid,
		DefaultCreditExpiryHours: &hours,
	}
}

// GetAccountSettings returns the stored settings for (payer, currency), or a
// DefaultAccountSettings value when none exists (never nil, never an error for a
// missing row).
func (s *MoneyService) GetAccountSettings(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	return s.getAccountSettings(ctx, payer, currency)
}

// getAccountSettings is the currency-aware form (#472).
func (s *MoneyService) getAccountSettings(ctx context.Context, payer identity.CustomerID, currency string) (*models.MoneyAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	cur := normalizeCurrency(currency)
	// Account settings / owed / auto-topup are billing-layer (#475 invariant):
	// custom credit units are never billed in.
	if err := RequireBillingCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	row, err := s.db.Gen(ctx).GetMoneyAccountSettings(ctx, gen.GetMoneyAccountSettingsParams{
		MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur,
	})
	if err == nil {
		return settingsFromGen(row), nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		d := DefaultAccountSettings(payer)
		d.MerchantID = tenantID
		d.Currency = cur
		return d, nil
	}
	return nil, err
}

// AccountSettingsInput is the upsert payload for an payer's spend policy. Only
// non-nil fields are written; nil fields keep their default / existing value.
type AccountSettingsInput struct {
	BillingMode              *string
	LowBalanceThreshold      *int64
	AutoTopupEnabled         *bool
	AutoTopupAmount          *int64
	AutoTopupPaymentMethod   *uuid.UUID
	DefaultCreditExpiryHours *int
}

// UpsertAccountSettings creates or updates the spend policy for (payer,
// currency). Validates the billing mode and alert threshold.
func (s *MoneyService) UpsertAccountSettings(ctx context.Context, payer identity.CustomerID, currency string, in AccountSettingsInput) (*models.MoneyAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *models.MoneyAccount
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := ensureCustomer(ctx, q, tid.UUID(), payer.UUID()); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: payer.UUID(), MerchantID: tid.UUID()}); err != nil {
			return err
		}
		var err error
		out, err = NewMoneyService(s.db.NewWithPgxTx(tx), s.clock).upsertAccountSettingsTx(ctx, payer, currency, in)
		return err
	})
	return out, err
}

func (s *MoneyService) upsertAccountSettingsTx(ctx context.Context, payer identity.CustomerID, currency string, in AccountSettingsInput) (*models.MoneyAccount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// Materialize the payable customers row before the subject's first settings
	// write.
	if err := ensureCustomer(ctx, s.db.Gen(ctx), tid.UUID(), payer.UUID()); err != nil {
		return nil, err
	}
	if in.BillingMode != nil {
		m := strings.ToLower(strings.TrimSpace(*in.BillingMode))
		if m != BillingModePrepaid && m != BillingModeArrears {
			return nil, fmt.Errorf("invalid billing_mode %q", *in.BillingMode)
		}
		in.BillingMode = &m
	}
	tenantID := tid.UUID()
	now := s.now()

	cur, err := s.GetAccountSettings(ctx, payer, currency)
	if err != nil {
		return nil, err
	}
	wasEnabled := cur.AutoTopupEnabled
	// Apply overrides onto the current/default view.
	if in.BillingMode != nil {
		cur.BillingMode = *in.BillingMode
	}
	if in.LowBalanceThreshold != nil {
		cur.LowBalanceThreshold = nilIfNeg(in.LowBalanceThreshold)
	}
	if in.AutoTopupEnabled != nil {
		cur.AutoTopupEnabled = *in.AutoTopupEnabled
	}
	if in.AutoTopupAmount != nil {
		cur.AutoTopupAmount = nilIfNeg(in.AutoTopupAmount)
	}
	if in.AutoTopupPaymentMethod != nil {
		cur.AutoTopupPaymentMethod = in.AutoTopupPaymentMethod
	}
	if in.DefaultCreditExpiryHours != nil {
		cur.DefaultCreditExpiryHours = in.DefaultCreditExpiryHours
	}

	cur.MerchantID = tenantID
	cur.CustomerID = payer.UUID()
	cur.Currency = normalizeCurrency(currency)
	// #474 invariant: money_accounts (billing settings) are external-currency-only.
	if err := RequireBillingCurrency(cur.Currency); err != nil {
		return nil, err
	}
	cur.UpdatedAt = now
	if cur.CreatedAt.IsZero() {
		cur.CreatedAt = now
	}

	var expiry *int32
	if cur.DefaultCreditExpiryHours != nil {
		v, _ := safecast.Convert[int32](*cur.DefaultCreditExpiryHours)
		expiry = &v
	}
	if err := s.db.Gen(ctx).UpsertMoneyAccountSettings(ctx, gen.UpsertMoneyAccountSettingsParams{
		MerchantID:               cur.MerchantID,
		CustomerID:               cur.CustomerID,
		Currency:                 cur.Currency,
		BillingMode:              cur.BillingMode,
		LowBalanceThreshold:      cur.LowBalanceThreshold,
		AutoTopupEnabled:         cur.AutoTopupEnabled,
		AutoTopupAmount:          cur.AutoTopupAmount,
		AutoTopupPaymentMethodID: cur.AutoTopupPaymentMethod,
		DefaultCreditExpiryHours: expiry,
		CreatedAt:                cur.CreatedAt,
		UpdatedAt:                cur.UpdatedAt,
	}); err != nil {
		return nil, err
	}
	if in.AutoTopupEnabled != nil && *in.AutoTopupEnabled && !wasEnabled {
		if err := s.db.Gen(ctx).ResetAutoTopupFailures(ctx, gen.ResetAutoTopupFailuresParams{MerchantID: tenantID, CustomerID: payer.UUID(), Currency: cur.Currency}); err != nil {
			return nil, err
		}
	}
	return s.GetAccountSettings(ctx, payer, currency)
}

func nilIfNeg(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	return v
}
