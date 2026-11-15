package solanasubs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// ChangeType is a plan change awaiting the transaction the subscriber's
// wallet signs: one durable operation per prepared transaction.
const ChangeType = "solana_tier_change"

// ChangeKey addresses the pending change of one prepared transaction.
func ChangeKey(reference string) string { return ChangeType + ":" + reference }

// Change is the agreement a plan change freezes before its transaction is
// handed to the wallet: what the transaction must do, and the reference and
// last valid block height it is looked up by on chain.
type Change struct {
	Reference            string `json:"reference"`
	LastValidBlockHeight uint64 `json:"last_valid_block_height"`

	OldSubscriptionID uuid.UUID  `json:"old_subscription_id"`
	CustomerID        uuid.UUID  `json:"customer_id"`
	OldPeriodEndsAt   *time.Time `json:"old_period_ends_at"`

	PriceID            uuid.UUID `json:"price_id"`
	ProductID          uuid.UUID `json:"product_id"`
	NewSubscriptionPDA string    `json:"new_subscription_pda"`
	PlanOwner          string    `json:"plan_owner"`
	Treasury           string    `json:"treasury"`
	PlanID             uint64    `json:"plan_id"`
	MintSymbol         string    `json:"mint_symbol"`
	AmountBaseUnits    uint64    `json:"amount_base_units"`
	PeriodHours        uint64    `json:"period_hours"`
	PlanCreatedAt      int64     `json:"plan_created_at"`
	Amount             int64     `json:"amount"`
	Currency           string    `json:"currency"`

	// An upgrade pulls its prorated first charge in the transaction.
	Upgrade              bool   `json:"upgrade"`
	FirstChargeBaseUnits uint64 `json:"first_charge_base_units"`
	FirstChargeMicros    int64  `json:"first_charge_micros"`

	PreparedAt time.Time `json:"prepared_at"`
}

// DecodeChange reads a pending change's frozen agreement.
func DecodeChange(intent gen.BillingProviderIntent) (Change, error) {
	var c Change
	if intent.IntentType != ChangeType || len(intent.Payload) == 0 {
		return c, errors.New("not a Solana plan change")
	}
	if err := json.Unmarshal(intent.Payload, &c); err != nil {
		return c, fmt.Errorf("decode Solana plan change: %w", err)
	}
	if c.Reference == "" || c.LastValidBlockHeight == 0 || c.OldSubscriptionID == uuid.Nil || c.PriceID == uuid.Nil || c.NewSubscriptionPDA == "" {
		return c, errors.New("Solana plan change is incomplete")
	}
	return c, nil
}

// pendingChangePage is one read of a subscription's plan changes.
const pendingChangePage = 100

// PendingChanges are the subscription's plan changes whose chain outcome is
// not settled: prepared, and neither recorded nor proven never to have landed.
func PendingChanges(ctx context.Context, d *db.DB, subscriptionID uuid.UUID) ([]gen.BillingProviderIntent, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	kind := ChangeType
	var pending []gen.BillingProviderIntent
	for offset := int64(0); ; offset += pendingChangePage {
		page, err := d.Gen(ctx).ListProviderIntents(ctx, gen.ListProviderIntentsParams{
			MerchantID: mid.UUID(), IntentType: &kind, SubscriptionID: &subscriptionID, PageOffset: offset, PageLimit: pendingChangePage,
		})
		if err != nil {
			return nil, err
		}
		for _, row := range page {
			switch row.Status {
			case "pending", "in_flight", "failed_retryable", "unknown_needs_verify":
				pending = append(pending, row)
			}
		}
		if len(page) < pendingChangePage {
			return pending, nil
		}
	}
}
