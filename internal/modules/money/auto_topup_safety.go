package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// AutoTopupEpisode identifies the existing provider intent, not a new charge.
type AutoTopupEpisode struct {
	IntentID, CustomerID, PaymentMethodID uuid.UUID
	Currency, Anchor                      string
	Amount                                int64
}

// AutoTopupReceipt holds a definitive provider response. A missing response is
// unknown, never a decline. The credit ledger remains accounting authority.
type AutoTopupReceipt struct {
	TransactionID string `json:"transaction_id,omitempty"`
	Declined      bool   `json:"declined,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

var ErrAutoTopupSafety = errors.New("auto-topup safety policy blocks a new charge")

func (s *MoneyService) autoTopupPolicy(ctx context.Context) (models.AutoTopupSafetyPolicy, error) {
	cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return models.AutoTopupSafetyPolicy{}, err
	}
	return merchantconfig.AutoTopupSafety(cfg.AutoTopupSafety)
}

func topupCounts(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, now time.Time) (gen.AutoTopupSafetyCountsRow, error) {
	return q.AutoTopupSafetyCounts(ctx, gen.AutoTopupSafetyCountsParams{MerchantID: merchantID, CustomerID: customerID, Currency: currency, DayStart: now.Add(-24 * time.Hour), WeekStart: now.Add(-7 * 24 * time.Hour), MonthStart: now.Add(-30 * 24 * time.Hour)})
}

// ReserveAutoTopup permits at most one first send. All charge writers lock the
// customer BEFORE settings, matching the money ledger lock order. Unknown
// reservations remain pending even outside the rolling windows.
func (s *MoneyService) ReserveAutoTopup(ctx context.Context, in AutoTopupEpisode) (existing *gen.OpenrailsAutoTopupEpisode, err error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := s.autoTopupPolicy(ctx)
	if err != nil {
		return nil, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: in.CustomerID, MerchantID: tid.UUID()}); err != nil {
			return err
		}
		st, err := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{MerchantID: tid.UUID(), CustomerID: in.CustomerID, Currency: normalizeCurrency(in.Currency)})
		if err != nil {
			return err
		}
		prior, err := q.GetAutoTopupEpisode(ctx, gen.GetAutoTopupEpisodeParams{MerchantID: tid.UUID(), IntentID: in.IntentID})
		if err == nil {
			existing = &prior
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		anchor := "genesis"
		if st.LastTopupAt != nil {
			anchor = strconv.FormatInt(st.LastTopupAt.UTC().Unix(), 10)
		}
		if !st.AutoTopupEnabled || st.AutoTopupAmount == nil || *st.AutoTopupAmount != in.Amount || st.AutoTopupPaymentMethodID == nil || *st.AutoTopupPaymentMethodID != in.PaymentMethodID || anchor != in.Anchor {
			return fmt.Errorf("%w: settings or episode changed", ErrAutoTopupSafety)
		}
		method, err := q.GetPaymentMethodByID(ctx, in.PaymentMethodID)
		if err != nil {
			return err
		}
		if method.CustomerID != in.CustomerID || method.MerchantID != tid.UUID() {
			return fmt.Errorf("%w: payment method ownership", ErrAutoTopupSafety)
		}
		now := s.now()
		counts, err := topupCounts(ctx, q, tid.UUID(), in.CustomerID, st.Currency, now)
		if err != nil {
			return err
		}
		if counts.Pending > 0 || counts.Daily >= int64(policy.MaxDaily) || counts.Weekly >= int64(policy.MaxWeekly) || counts.Monthly >= int64(policy.MaxMonthly) {
			return fmt.Errorf("%w: cap or unresolved episode", ErrAutoTopupSafety)
		}
		_, err = q.InsertAutoTopupEpisode(ctx, gen.InsertAutoTopupEpisodeParams{IntentID: in.IntentID, MerchantID: tid.UUID(), CustomerID: in.CustomerID, Currency: st.Currency, ReservedAt: now})
		return err
	})
	return existing, err
}

func (s *MoneyService) GetAutoTopupEpisode(ctx context.Context, id uuid.UUID) (*gen.OpenrailsAutoTopupEpisode, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetAutoTopupEpisode(ctx, gen.GetAutoTopupEpisodeParams{MerchantID: tid.UUID(), IntentID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// RecordAutoTopupReceipt never replaces the first exact provider receipt.
func (s *MoneyService) RecordAutoTopupReceipt(ctx context.Context, id uuid.UUID, receipt AutoTopupReceipt) error {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if !receipt.Declined && receipt.TransactionID == "" {
		return fmt.Errorf("confirmed topup transaction id required")
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	n, err := s.db.Gen(ctx).RecordAutoTopupReceipt(ctx, gen.RecordAutoTopupReceiptParams{MerchantID: tid.UUID(), IntentID: id, Receipt: data})
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	prior, err := s.GetAutoTopupEpisode(ctx, id)
	if err != nil {
		return err
	}
	if prior == nil || len(prior.Receipt) == 0 {
		return fmt.Errorf("topup reservation missing")
	}
	var saved AutoTopupReceipt
	if err := json.Unmarshal(prior.Receipt, &saved); err != nil {
		return err
	}
	if saved.Declined != receipt.Declined || saved.TransactionID != receipt.TransactionID {
		return fmt.Errorf("topup provider receipt conflicts")
	}
	return nil
}

// FinalizeAutoTopupReceipt atomically credits confirmed money or counts one
// definitive decline, advances the cooldown, and queues a disable notice once.
func (s *MoneyService) FinalizeAutoTopupReceipt(ctx context.Context, in AutoTopupEpisode) (receipt AutoTopupReceipt, err error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return receipt, err
	}
	policy, err := s.autoTopupPolicy(ctx)
	if err != nil {
		return receipt, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: in.CustomerID, MerchantID: tid.UUID()}); err != nil {
			return err
		}
		st, err := q.LockMoneyAccountSettings(ctx, gen.LockMoneyAccountSettingsParams{MerchantID: tid.UUID(), CustomerID: in.CustomerID, Currency: normalizeCurrency(in.Currency)})
		if err != nil {
			return err
		}
		ep, err := q.GetAutoTopupEpisode(ctx, gen.GetAutoTopupEpisodeParams{MerchantID: tid.UUID(), IntentID: in.IntentID})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(ep.Receipt, &receipt); err != nil {
			return fmt.Errorf("topup has no definitive receipt: %w", err)
		}
		if ep.FinalizedAt != nil {
			return nil
		}
		now := s.now()
		failures := int32(0)
		disable := false
		if receipt.Declined {
			failures = st.AutoTopupFailures + 1
			disable = int64(failures) >= int64(policy.DeclinesBeforeDisable)
		} else {
			if receipt.TransactionID == "" {
				return fmt.Errorf("topup receipt has no transaction id")
			}
			payer := identity.CustomerID(in.CustomerID)
			sourceID := "topup:" + in.IntentID.String()
			params := DepositParams{CustomerID: &payer, Invoker: payer.String(), Currency: in.Currency, Amount: in.Amount, Source: "auto_topup", SourceID: &sourceID}
			if st.DefaultCreditExpiryHours != nil && *st.DefaultCreditExpiryHours > 0 {
				expiry := now.Add(time.Duration(*st.DefaultCreditExpiryHours) * time.Hour)
				params.ExpiresAt = &expiry
			}
			if _, err := s.depositTx(ctx, q, params); err != nil {
				return err
			}
		}
		if err := q.CompleteAutoTopupAccount(ctx, gen.CompleteAutoTopupAccountParams{MerchantID: tid.UUID(), CustomerID: in.CustomerID, Currency: st.Currency, Now: now, Failures: failures, Disable: disable}); err != nil {
			return err
		}
		if disable {
			data, err := json.Marshal(map[string]any{"currency": st.Currency, "consecutive_declines": failures, "reason": receipt.Reason})
			if err != nil {
				return err
			}
			if err := q.CreateNotificationIfAbsent(ctx, gen.CreateNotificationIfAbsentParams{ID: in.IntentID, MerchantID: tid.UUID(), CustomerID: in.CustomerID, EventType: string(models.NotificationAutoTopupDisabled), Data: data, CreatedAt: now}); err != nil {
				return err
			}
		}
		_, err = q.FinalizeAutoTopupEpisode(ctx, gen.FinalizeAutoTopupEpisodeParams{MerchantID: tid.UUID(), IntentID: in.IntentID, Now: now})
		return err
	})
	return receipt, err
}
