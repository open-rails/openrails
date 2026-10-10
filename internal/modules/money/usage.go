package money

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// A usage event's outcome.
const (
	UsageSucceeded = "succeeded"
	UsageFailed    = "failed"
)

// ErrUsageOutsideIngestWindow refuses an event dated before the ingest window
// or ahead of the clock: its month may have no partition, and its key could
// not be looked up.
var ErrUsageOutsideIngestWindow = fmt.Errorf("occurred_at must be within the last %d days and not in the future", int(retention.UsageIngestWindow/retention.Day))

// usageKeyWindow is the occurred_at range an idempotency key is looked up in:
// every event that could have been recorded and still be honoured at now.
func usageKeyWindow(now time.Time) (from, to time.Time) {
	return now.Add(-retention.UsageIngestWindow), now.Add(retention.UsageClockSkew)
}

// RecordUsageParams is one metered, host-priced usage event (issue #289). The
// host supplies the final Amount (in the currency's internal precision); OpenRails
// records the event AND debits the ledger atomically.
type RecordUsageParams struct {
	// Payer is the merchant subject BILLED for this usage (the payer). When nil it is
	// resolved from Invoker (self-hosted/personal case), never synthesized.
	Payer     *identity.CustomerID
	Invoker   string // invoker (attribution only)
	Currency  string
	EventType string // metered event kind, e.g. "gpt-4o"
	// Dimensions are per-dimension counts (input_tokens, output_tokens,
	// cached_input_tokens, requests, ...). Used for reporting + #298 throughput.
	Dimensions map[string]int64
	// Amount is the host-priced cost (>= 0). 0 records a free/zero-cost event
	// (still metered for dimensions) without a ledger debit.
	Amount int64
	// Key is the idempotency coordinate; its operation must be
	// UsageOperation(EventType), so two different event types at one
	// (source, source_id) post two distinct charges (or#894).
	Key      IdempotencyKey
	Metadata map[string]any
	// OccurredAt defaults to now when zero. It must lie within the ingest
	// window (retention.UsageIngestWindow back, retention.UsageClockSkew ahead).
	OccurredAt time.Time
	// Failed marks usage that failed; Amount is what the failure cost. The
	// customer's own failure is forgiven up to FailedWindows (its grace) and
	// charged past them; no window forgives it all. A Delegated invoker's
	// failure is never charged and counts toward FailedWindows, its cutoff.
	Failed        bool
	Delegated     bool
	FailedWindows []FailedUsageWindow
}

// RecordUsage durably records a metered usage event AND debits the credit ledger
// in ONE transaction (issue #289). Idempotent on
// (merchant, payer, event_type, source, source_id): a replayed request returns the
// existing event with Replayed set and never double-charges, while a replay
// carrying a CHANGED amount is refused (ErrIdempotencyKeyReused). The event's
// key is honoured for the ingest window after it occurred; the ledger debit of
// a priced event is idempotent for good.
// Concurrency-safe: the balance row is locked FOR UPDATE before the idempotency
// check, so two concurrent identical records serialize and the second sees the
// first's event.
//
// A zero Amount is a legitimate use: it records the event durably (metering
// dimensions, and or#903's once-only claim for a report whose money outcome is
// nothing) without touching the ledger.
//
// This is the DURABLE side. In FAST mode (#298) it runs write-behind, off the
// per-request hot path; the synchronous admission decision is the Redis headroom
// op, not this write.
func (s *MoneyService) RecordUsage(ctx context.Context, params RecordUsageParams) (*models.UsageEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	params.EventType = strings.TrimSpace(params.EventType)
	if params.EventType == "" {
		return nil, fmt.Errorf("event_type required")
	}
	if err := params.Key.RequireOperation(UsageOperation(params.EventType)); err != nil {
		return nil, err
	}
	if params.Amount < 0 {
		return nil, fmt.Errorf("amount must be >= 0")
	}
	cur := normalizeCurrency(params.Currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	payer, err := resolveCustomer(params.Payer, params.Invoker)
	if err != nil {
		return nil, err
	}
	if params.Delegated && (!params.Failed || strings.TrimSpace(params.Invoker) == "") {
		return nil, fmt.Errorf("a delegated invoker's usage is failed usage naming its invoker")
	}

	s.db.EnsurePartitions(ctx, s.now())
	var ev *models.UsageEvent
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		tid, terr := merchant.Require(ctx)
		if terr != nil {
			return terr
		}
		tenantID := tid.UUID()
		payerID := payer.UUID()
		now := s.now()
		occurred := params.OccurredAt.UTC()
		if params.OccurredAt.IsZero() {
			occurred = now
		}
		keyFrom, keyTo := usageKeyWindow(now)
		if occurred.Before(keyFrom) || occurred.After(keyTo) {
			return ErrUsageOutsideIngestWindow
		}

		// Serialize per (merchant, payer) so the idempotency check below
		// can't race a concurrent identical record into a double charge.
		if _, err := s.lockBalance(ctx, q, payer, params.Invoker, cur); err != nil {
			return err
		}

		// A failed event's amount is final: it is never rated by the catalog.
		pricingAuthority := "catalog"
		if params.Amount > 0 || params.Failed {
			pricingAuthority = "host"
		}
		outcome := UsageSucceeded
		if params.Failed {
			outcome = UsageFailed
		}
		// or#891 item 3: same key, different charging term = refusal, never the
		// first event answered for a corrected one.
		replay := func(row gen.BillingUsageEvent) error {
			var rerr error
			if ev, rerr = usageEventFromGen(row); rerr != nil {
				return rerr
			}
			if ev.Outcome != outcome {
				return &IdempotencyConflict{
					Operation: string(params.Key.Operation()), Source: params.Key.Source(), SourceID: params.Key.SourceID(),
					Field: "outcome", Committed: ev.Outcome, Retried: outcome,
				}
			}
			if ev.PricingAuthority != pricingAuthority {
				return &IdempotencyConflict{
					Operation: string(params.Key.Operation()), Source: params.Key.Source(), SourceID: params.Key.SourceID(),
					Field: "pricing_authority", Committed: ev.PricingAuthority, Retried: pricingAuthority,
				}
			}
			// A failed event charged what its grace left: the host reported
			// its amount plus what was forgiven.
			if reported := ev.Amount + ev.ForgivenAmount; reported != params.Amount {
				return &IdempotencyConflict{
					Operation: string(params.Key.Operation()), Source: params.Key.Source(), SourceID: params.Key.SourceID(),
					Field: "amount", Committed: reported, Retried: params.Amount,
				}
			}
			// or#903: say so. A caller that must not repeat a NON-ledger side
			// effect (a cache counter, a host notification) cannot tell an
			// applied write from a replayed one without this.
			ev.Replayed = true
			return nil
		}

		coords := gen.GetUsageEventByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID,
			Currency:  cur,
			EventType: params.EventType, Source: params.Key.Source(), SourceID: params.Key.SourceID(),
			OccurredFrom: keyFrom, OccurredTo: keyTo,
		}
		existingRow, gerr := q.GetUsageEventByCoords(ctx, coords)
		if gerr == nil {
			return replay(existingRow)
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		// A failure is charged what its grace does not forgive, and counts
		// toward its windows in this transaction.
		charge, forgiven := params.Amount, int64(0)
		if params.Failed {
			periods, perr := failedUsagePeriods(params.FailedWindows, now)
			if perr != nil {
				return perr
			}
			key := failedUsageKey{merchant: tenantID, customer: payerID, currency: cur}
			switch {
			case params.Delegated:
				key.invoker = params.Invoker
				charge = 0
			case len(periods) == 0:
				charge = 0
			default:
				left, gerr := key.graceLeft(ctx, q, periods)
				if gerr != nil {
					return gerr
				}
				charge = max(params.Amount-left, 0)
			}
			forgiven = params.Amount - charge
			if err := key.count(ctx, q, periods, params.Amount, now); err != nil {
				return err
			}
		}

		// Debit the ledger for the host-priced amount (skip for zero-cost events).
		// Unified credit line (#302): draw prepaid balance first, then accrue to
		// owed up to the credit line. Prepay-only accounts (no line) deny when the
		// amount exceeds available balance.
		var debitID *uuid.UUID
		if charge == 0 {
			// A metered payer's first event opens its balance account: the
			// permanent first-activity mark invoice periods are anchored on.
			if _, err := ledger.New(q, tenantID).EnsureCustomerBalance(ctx, payerID, cur); err != nil {
				return err
			}
		}
		if charge > 0 {
			_, _, applied, derr := s.spendBalanceThenOwedTx(ctx, q, payer, params.Invoker, cur, params.Key, charge, false)
			if derr != nil {
				return derr
			}
			// Link the usage event to the durable #512 spend transfer (the balance
			// debit, else the owed accrual) at these coordinates.
			tr, terr := q.GetLedgerSpendByCoords(ctx, gen.GetLedgerSpendByCoordsParams{
				MerchantID: tenantID, CustomerID: payerID, Currency: cur,
				Operation: string(params.Key.Operation()),
				Source:    params.Key.Source(), SourceID: params.Key.SourceID(),
			})
			if terr != nil && !errors.Is(terr, pgx.ErrNoRows) {
				return terr
			}
			if terr == nil {
				id := tr.ID
				debitID = &id
				if !applied {
					// The ledger debit is permanent and older than the key
					// window: its event lies within the window around it.
					coords.OccurredFrom, coords.OccurredTo = usageKeyWindow(tr.CreatedAt)
					row, rerr := q.GetUsageEventByCoords(ctx, coords)
					if rerr == nil {
						return replay(row)
					}
					if !errors.Is(rerr, pgx.ErrNoRows) {
						return rerr
					}
				}
			}
		}

		ev = &models.UsageEvent{
			ID:               uuidutil.NewV7(),
			MerchantID:       tenantID,
			CustomerID:       payerID,
			Invoker:          params.Invoker,
			Currency:         cur,
			EventType:        params.EventType,
			Dimensions:       params.Dimensions,
			Amount:           charge,
			ForgivenAmount:   forgiven,
			Outcome:          outcome,
			Source:           params.Key.Source(),
			SourceID:         params.Key.SourceID(),
			LedgerTransferID: debitID,
			PricingAuthority: pricingAuthority,
			Metadata:         params.Metadata,
			OccurredAt:       occurred,
			CreatedAt:        now,
		}
		dims, jerr := toJSONBC(ev.Dimensions)
		if jerr != nil {
			return jerr
		}
		meta, jerr := toJSONBC(ev.Metadata)
		if jerr != nil {
			return jerr
		}
		return q.InsertUsageEvent(ctx, gen.InsertUsageEventParams{
			ID:               ev.ID,
			MerchantID:       ev.MerchantID,
			CustomerID:       ev.CustomerID,
			InvokerID:        ev.Invoker,
			Currency:         ev.Currency,
			Resource:         ev.Resource,
			EventType:        ev.EventType,
			Dimensions:       dims,
			Amount:           ev.Amount,
			Source:           ev.Source,
			SourceID:         ev.SourceID,
			LedgerTransferID: ev.LedgerTransferID,
			PricingAuthority: pricingAuthority,
			Metadata:         meta,
			OccurredAt:       ev.OccurredAt,
			CreatedAt:        ev.CreatedAt,
			Outcome:          ev.Outcome,
			ForgivenAmount:   ev.ForgivenAmount,
		})
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// FindUsageEvent returns the durable event already recorded at these
// idempotency coordinates, or (nil, nil) when the key is unclaimed.
//
// or#903: this is how a caller asks "has my write already happened?" BEFORE
// deciding what to write. It exists because some callers derive the amount they
// are about to record from mutable state (wasted-spend grace), so re-deriving it
// on a replay produces a different number and RecordUsage's changed-amount
// refusal would fire on an IDENTICAL retry. The read is not a lock — RecordUsage
// remains the atomic decision — it only lets the caller stop before it grades.
func (s *MoneyService) FindUsageEvent(ctx context.Context, payer identity.CustomerID, currency, eventType string, key IdempotencyKey) (*models.UsageEvent, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return nil, fmt.Errorf("event_type required")
	}
	if err := key.RequireOperation(UsageOperation(eventType)); err != nil {
		return nil, err
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var ev *models.UsageEvent
	keyFrom, keyTo := usageKeyWindow(s.now())
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, gerr := s.db.Gen(ctx).GetUsageEventByCoords(ctx, gen.GetUsageEventByCoordsParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(),
			Currency:  cur,
			EventType: eventType, Source: key.Source(), SourceID: key.SourceID(),
			OccurredFrom: keyFrom, OccurredTo: keyTo,
		})
		if errors.Is(gerr, pgx.ErrNoRows) {
			return nil
		}
		if gerr != nil {
			return gerr
		}
		ev, gerr = usageEventFromGen(row)
		if gerr != nil {
			return gerr
		}
		ev.Replayed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ev, nil
}

// UsageRollupRow is a per-event_type aggregate of usage over a window: total
// host-priced amount, event count, and summed per-dimension counts. Powers
// usage reporting (GET /v1/me/usage) and #303 invoice line items.
type UsageRollupRow struct {
	EventType   string           `json:"event_type"`
	Currency    string           `json:"currency"`
	TotalAmount int64            `json:"total_amount"`
	EventCount  int64            `json:"event_count"`
	Dimensions  map[string]int64 `json:"dimensions"`
}

// AggregateUsage rolls up an payer's usage_events over [from, to) grouped by
// event_type, with summed dimensions. Scoped to the request merchant. This is
// the rollup layer — it is NEVER called on the per-request admission hot path.
func (s *MoneyService) AggregateUsage(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) ([]UsageRollupRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur := normalizeCurrency(currency)
	if err := moneyutil.ValidateCurrency(cur); err != nil {
		return nil, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	rows := map[string]*UsageRollupRow{}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		totals, err := q.AggregateUsageTotals(ctx, gen.AggregateUsageTotalsParams{
			MerchantID: tenantID, CustomerID: payerID,
			Currency: cur,
			FromAt:   from.UTC(), ToAt: to.UTC(),
		})
		if err != nil {
			return err
		}
		for _, t := range totals {
			rows[t.EventType] = &UsageRollupRow{
				EventType:   t.EventType,
				Currency:    cur,
				TotalAmount: t.TotalAmount,
				EventCount:  t.EventCount,
				Dimensions:  map[string]int64{},
			}
		}

		dims, err := q.AggregateUsageDimensions(ctx, gen.AggregateUsageDimensionsParams{
			MerchantID: tenantID, CustomerID: payerID,
			Currency: cur,
			FromAt:   from.UTC(), ToAt: to.UTC(),
		})
		if err != nil {
			return err
		}
		for _, d := range dims {
			if r, ok := rows[d.EventType]; ok {
				r.Dimensions[d.Key] = d.Total
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]UsageRollupRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	return out, nil
}
