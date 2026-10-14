package observed

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Execution identifies the actual signed attempt saved before submission.
// It is optional: externally observed provider bills invent no local attempt.
type Execution struct {
	IntentID     uuid.UUID `json:"intent_id"`
	AttemptID    uuid.UUID `json:"attempt_id"`
	ClaimAttempt int32     `json:"claim_attempt"`
	Signature    string    `json:"signature"`
	SubmittedAt  time.Time `json:"submitted_at"`
}

func verifyExecution(ctx context.Context, q *gen.Queries, mid uuid.UUID, b Bill) error {
	e := b.Execution
	if e == nil {
		return nil
	}
	if b.Owner != OwnerEngine || b.SourceKind != "solana_pull" || b.SourceKey != e.IntentID.String() || b.Payment == nil || b.Payment.Rail != models.RailSolana || b.Payment.Amount != b.Total || e.IntentID == uuid.Nil || e.AttemptID == uuid.Nil || e.ClaimAttempt <= 0 || e.SubmittedAt.IsZero() || e.Signature != b.Payment.TransactionID || !b.Settled || b.SettlementSource != "cash" {
		return ErrConflict
	}
	in, err := q.GetProviderIntent(ctx, gen.GetProviderIntentParams{MerchantID: mid, ID: e.IntentID})
	if err != nil {
		return err
	}
	if in.IntentType != "solana_pull" || in.MerchantID != mid || !sameID(in.SubscriptionID, b.SubscriptionID) || in.PspID == nil || *in.PspID != b.PSPID {
		return ErrConflict
	}
	var evidence struct {
		TransactionID string     `json:"transaction_id"`
		Execution     *Execution `json:"execution"`
	}
	if json.Unmarshal(in.ResultEvidence, &evidence) != nil || evidence.Execution == nil {
		return ErrConflict
	}
	stored := evidence.Execution
	if evidence.TransactionID != e.Signature || stored.IntentID != e.IntentID || stored.AttemptID != e.AttemptID || stored.ClaimAttempt != e.ClaimAttempt || stored.Signature != e.Signature || !stored.SubmittedAt.Equal(e.SubmittedAt) {
		return ErrConflict
	}
	var payload struct {
		Agreement struct {
			PriceID               uuid.UUID `json:"price_id"`
			ProductID             uuid.UUID `json:"product_id"`
			CustomerID            uuid.UUID `json:"customer_id"`
			Amount                int64     `json:"amount,string"`
			Currency              string    `json:"currency"`
			Description           string    `json:"description"`
			PeriodHours           uint64    `json:"period_hours,string"`
			EpisodeAt             time.Time `json:"episode_at"`
			Quantity              *int      `json:"quantity"`
			AccessDurationHours   *int      `json:"access_duration_hours"`
			ProviderAccessDefault bool      `json:"provider_access_default,omitempty"`
		} `json:"agreement"`
	}
	if json.Unmarshal(in.Payload, &payload) != nil || len(b.Lines) != 1 {
		return ErrConflict
	}
	a, line := payload.Agreement, b.Lines[0]
	if a.PeriodHours == 0 || a.PeriodHours > 24*366 {
		return ErrConflict
	}
	if a.CustomerID != b.CustomerID || a.Amount != b.Total || a.Currency != b.Currency || !a.EpisodeAt.Equal(b.DueAt) || !sameID(line.PriceID, &a.PriceID) || !sameID(line.ProductID, &a.ProductID) || line.Description != a.Description || !sameQuantity(line.Quantity, a.Quantity) || !sameAccessHours(line.AccessDurationHours, a.AccessDurationHours) || a.ProviderAccessDefault != line.ProviderAccessDefault || line.BillingIntervalHours == nil || *line.BillingIntervalHours != int(a.PeriodHours) || b.PeriodEnd.Sub(b.PeriodStart) != time.Duration(a.PeriodHours)*time.Hour {
		return ErrConflict
	}
	return nil
}

func sameAccessHours(a, b *int) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func recordExecution(ctx context.Context, q *gen.Queries, mid uuid.UUID, b Bill, result Result, now time.Time) error {
	e := b.Execution
	if e == nil {
		return nil
	}
	if result.Payment == nil {
		return errors.New("verified execution has no recorded payment")
	}
	row, err := q.GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid, ID: e.AttemptID})
	if db.IsNotFound(err) {
		state, encodeErr := json.Marshal(map[string]any{"operation_id": e.IntentID.String(), "solana_execution": e})
		if encodeErr != nil {
			return encodeErr
		}
		if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{MerchantID: mid, ID: e.AttemptID, CustomerID: b.CustomerID, OrderID: result.Order.ID,
			Rail: string(models.RailSolana), Status: "processing", Amount: b.Total, Currency: b.Currency, ExpiresAt: b.PeriodEnd,
			RailState: state, PspID: b.PSPID, Now: e.SubmittedAt}); err != nil {
			return err
		}
		row, err = q.GetOrderAttempt(ctx, gen.GetOrderAttemptParams{MerchantID: mid, ID: e.AttemptID})
	}
	if err != nil {
		return err
	}
	if row.OrderID == nil || *row.OrderID != result.Order.ID || row.CustomerID != b.CustomerID || row.PspID != b.PSPID || row.Amount == nil || *row.Amount != b.Total || row.Currency == nil || *row.Currency != b.Currency || !row.CreatedAt.Equal(e.SubmittedAt) {
		return ErrConflict
	}
	if row.Status == "succeeded" {
		if row.PaymentID == nil || *row.PaymentID != result.Payment.ID || row.TransactionID == nil || *row.TransactionID != e.Signature {
			return ErrConflict
		}
		return nil
	}
	if row.Status != "processing" {
		return ErrConflict
	}
	changed, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: mid, ID: e.AttemptID, Status: "succeeded", PaymentID: &result.Payment.ID, TransactionID: &e.Signature, Now: now})
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	if result.Order.Status == "open" {
		changed, err := q.StartOrderAttempt(ctx, gen.StartOrderAttemptParams{MerchantID: mid, ID: result.Order.ID, AttemptID: e.AttemptID, PspID: b.PSPID, Now: now})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrConflict
		}
	}
	return nil
}
