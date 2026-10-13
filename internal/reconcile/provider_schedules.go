package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A live provider schedule imported without proven terms (an open
// life.import.schedule_unresolved finding) takes them from the provider's own
// recurring record the next time a pull reads it: the record's amount and day
// frequency, anchored on its next charge date, when they match the
// subscription's price. The subscription's unresolved receipts are then read
// again from the provider by id and placed as the accepted schedule places
// them: settled history before its first cycle, or the cycle they pay. A
// record that disagrees, or no record, keeps the finding open with the
// provider's evidence. Host and legacy rows never shape the schedule. Only
// NMI's record states recurring terms today; a rail whose pull reads none
// (CCBill) keeps its schedules unresolved.

// TransactionReader reads provider transactions by id.
type TransactionReader interface {
	TransactionsByID(ctx context.Context, ids []string) (map[string]RemoteTransaction, error)
}

// ProviderScheduleResult counts one pull's schedule acceptances.
type ProviderScheduleResult struct{ Accepted, Unresolved, Replayed int }

const (
	unresolvedSchedulePage = 500
	// receiptReplayChunk subscriptions share one read of their unresolved
	// receipts, at most unresolvedReceiptLimit of them.
	receiptReplayChunk     = 100
	unresolvedReceiptLimit = 10_000
)

// providerScheduleSource is the source reference of a schedule accepted from
// the provider's recurring record.
func providerScheduleSource(rail models.Rail, railSubscriptionID string) string {
	return string(rail) + "_subscription:" + railSubscriptionID
}

type acceptedSchedule struct {
	sub     *models.Subscription
	finding uuid.UUID
	entry   *RemoteSubscription
	source  string
}

// AcceptProviderSchedules evaluates the PSP's subscriptions with an
// unresolved imported schedule against this read of the provider's roster.
// exhaustive says the roster proves absence. reader reads receipts by id; nil
// leaves accepted subscriptions' receipts for a later pass.
func (w *PGLocalWriter) AcceptProviderSchedules(ctx context.Context, psp uuid.UUID, snap *RemoteSnapshot, exhaustive bool, reader TransactionReader) (ProviderScheduleResult, error) {
	var res ProviderScheduleResult
	mid, err := merchant.Require(ctx)
	if err != nil {
		return res, err
	}
	roster := make(map[string]*RemoteSubscription, len(snap.Subscriptions))
	for i := range snap.Subscriptions {
		roster[snap.Subscriptions[i].RailSubscriptionID] = &snap.Subscriptions[i]
	}
	q := w.DB.Gen(ctx)
	repo := subscriptions.NewSubscriptionRepo(w.DB)
	prices := map[uuid.UUID]gen.BillingPrice{}
	now := w.now()
	var errs error
	for after := ""; ; {
		page, err := q.ListUnresolvedScheduleSubscriptions(ctx, gen.ListUnresolvedScheduleSubscriptionsParams{MerchantID: mid.UUID(), After: after, PspID: psp, RowLimit: unresolvedSchedulePage})
		if err != nil {
			return res, fmt.Errorf("list unresolved schedules: %w", err)
		}
		var accepted []acceptedSchedule
		for _, row := range page {
			after = row.SubjectKey
			sub, err := repo.GetByID(ctx, row.SubscriptionID)
			if err != nil {
				return res, fmt.Errorf("load %s: %w", billing.SubscriptionID(row.SubscriptionID), err)
			}
			if !sub.Status.Live() || sub.RailSubscriptionID == "" {
				continue
			}
			entry := roster[sub.RailSubscriptionID]
			source, err := w.acceptProviderSchedule(ctx, sub, entry, exhaustive, prices, now)
			if err != nil {
				errs = errors.Join(errs, fmt.Errorf("%s: %w", billing.SubscriptionID(sub.ID), err))
				continue
			}
			if source == "" {
				res.Unresolved++
				continue
			}
			accepted = append(accepted, acceptedSchedule{sub: sub, finding: row.FindingID, entry: entry, source: source})
		}
		for len(accepted) > 0 {
			n := min(len(accepted), receiptReplayChunk)
			replayed, err := w.replayUnresolvedReceipts(ctx, psp, accepted[:n], reader, now)
			res.Accepted, res.Replayed = res.Accepted+replayed.Accepted, res.Replayed+replayed.Replayed
			errs = errors.Join(errs, err)
			accepted = accepted[n:]
		}
		if len(page) < unresolvedSchedulePage {
			return res, errs
		}
	}
}

// acceptProviderSchedule answers the source reference of the subscription's
// accepted schedule, accepting one from the provider's record when it has
// none; "" when the record cannot be accepted (the finding says why).
func (w *PGLocalWriter) acceptProviderSchedule(ctx context.Context, sub *models.Subscription, entry *RemoteSubscription, exhaustive bool, prices map[uuid.UUID]gen.BillingPrice, now time.Time) (string, error) {
	q := w.DB.Gen(ctx)
	// Accepted already (an earlier pass, or a declaration since): only its
	// receipts remain.
	existing, err := q.ListProviderSchedules(ctx, gen.ListProviderSchedulesParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, ProviderReference: sub.RailSubscriptionID})
	if err != nil || len(existing) > 0 {
		if err != nil {
			return "", err
		}
		return existing[0].SourceReference, nil
	}
	if entry == nil {
		if !exhaustive {
			return "", nil
		}
		return "", recordUnresolvedSchedule(ctx, q, sub, "the provider holds no recurring record of this schedule", nil)
	}
	if entry.Amount == "" && entry.IntervalDays == 0 && entry.IntervalMonths == 0 {
		return "", nil // this rail's read states no recurring terms
	}
	price, ok := prices[sub.PriceID]
	if !ok {
		if price, err = q.GetPriceByID(ctx, gen.GetPriceByIDParams{MerchantID: sub.MerchantID, ID: sub.PriceID}); err != nil {
			return "", fmt.Errorf("load price: %w", err)
		}
		prices[sub.PriceID] = price
	}
	amount, hours, reason := providerTerms(entry, price, sub.Quantity)
	if reason != "" {
		return "", recordUnresolvedSchedule(ctx, q, sub, reason, providerRecordEvidence(sub.Rail, entry))
	}
	source := providerScheduleSource(sub.Rail, sub.RailSubscriptionID)
	err = w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := w.DB.NewWithPgxTx(tx)
		locked, err := subscriptions.NewSubscriptionRepo(d).GetByIDForUpdate(ctx, sub.ID)
		if err != nil {
			return err
		}
		schedules, err := d.Gen(ctx).ListProviderSchedules(ctx, gen.ListProviderSchedulesParams{MerchantID: locked.MerchantID, SubscriptionID: locked.ID, ProviderReference: locked.RailSubscriptionID})
		if err != nil || len(schedules) > 0 {
			if err == nil {
				source = schedules[0].SourceReference
			}
			return err
		}
		bill, err := scheduleBill(ctx, d.Gen(ctx), locked, price, price.Currency, amount, locked.Quantity, hours)
		if err != nil {
			return err
		}
		return observed.DeclareSchedule(ctx, d, source, bill, *entry.NextBillingAt, hours, 0, now)
	})
	if err != nil {
		return "", err
	}
	return source, nil
}

// providerTerms reads the provider's recurring record against the price the
// subscription bills: the same amount for its seats and the same whole-day
// cadence, with a next charge date. A reason names the first disagreement.
func providerTerms(entry *RemoteSubscription, price gen.BillingPrice, quantity *int) (amount int64, hours int, reason string) {
	if entry.Paused {
		return 0, 0, "the provider's schedule is paused"
	}
	if !remoteLive(entry.Status) || entry.NextBillingAt == nil {
		return 0, 0, "the provider's schedule states no next charge date"
	}
	if price.BillingIntervalHours == nil || *price.BillingIntervalHours <= 0 || *price.BillingIntervalHours%24 != 0 {
		return 0, 0, "the subscription's price has no whole-day billing cadence"
	}
	hours = int(*price.BillingIntervalHours)
	switch {
	case entry.IntervalMonths > 0:
		return 0, 0, fmt.Sprintf("the provider's schedule bills every %d month(s); the price every %s", entry.IntervalMonths, cadence.FormatHours(hours))
	case entry.IntervalDays <= 0:
		return 0, 0, "the provider's schedule states no billing frequency"
	case entry.IntervalDays*24 != hours:
		return 0, 0, fmt.Sprintf("the provider's schedule bills every %s; the price every %s", cadence.FormatHours(entry.IntervalDays*24), cadence.FormatHours(hours))
	}
	seats := int64(1)
	if quantity != nil && *quantity > 0 {
		seats = int64(*quantity)
	}
	want := price.Amount * seats
	currency := moneyutil.NormalizeCurrency(price.Currency)
	minor, err := moneyutil.DecimalToRailMinor(currency, entry.Amount)
	if err == nil {
		amount, err = moneyutil.RailMinorToNative(currency, minor)
	}
	if err != nil || amount != want {
		return 0, 0, fmt.Sprintf("the provider's schedule bills %q %s; the price bills %s", entry.Amount, currency, moneyutil.FormatAmount(want, currency))
	}
	return amount, hours, ""
}

// providerRecord is the provider's recurring record as a finding states it.
type providerRecord struct {
	Source             string     `json:"source"`
	RailSubscriptionID string     `json:"rail_subscription_id"`
	PlanID             string     `json:"plan_id,omitempty"`
	Amount             string     `json:"amount"`
	IntervalDays       int        `json:"interval_days"`
	IntervalMonths     int        `json:"interval_months"`
	NextBillingAt      *time.Time `json:"next_billing_at,omitempty"`
	Paused             bool       `json:"paused"`
}

func providerRecordEvidence(rail models.Rail, entry *RemoteSubscription) *providerRecord {
	return &providerRecord{Source: string(rail) + "_subscription", RailSubscriptionID: entry.RailSubscriptionID, PlanID: entry.PlanID, Amount: entry.Amount,
		IntervalDays: entry.IntervalDays, IntervalMonths: entry.IntervalMonths, NextBillingAt: entry.NextBillingAt, Paused: entry.Paused}
}

// replayUnresolvedReceipts reads the accepted subscriptions' unresolved
// receipts from the provider by id and records each as its schedule places
// it, then resolves each subscription's finding. A receipt the provider does
// not report as a sale or decline stays unresolved, with why.
func (w *PGLocalWriter) replayUnresolvedReceipts(ctx context.Context, psp uuid.UUID, accepted []acceptedSchedule, reader TransactionReader, now time.Time) (ProviderScheduleResult, error) {
	var res ProviderScheduleResult
	q := w.DB.Gen(ctx)
	ids := make([]string, len(accepted))
	for i, a := range accepted {
		ids[i] = billing.SubscriptionID(a.sub.ID).String()
	}
	rows, err := q.ListUnresolvedReceiptFindings(ctx, gen.ListUnresolvedReceiptFindingsParams{MerchantID: accepted[0].sub.MerchantID, PspID: psp, SubscriptionIds: ids, RowLimit: unresolvedReceiptLimit})
	if err != nil {
		return res, fmt.Errorf("list unresolved receipts: %w", err)
	}
	if len(rows) == unresolvedReceiptLimit {
		return res, fmt.Errorf("more than %d unresolved receipts across %d subscriptions", unresolvedReceiptLimit, len(accepted))
	}
	receipts := map[string][]gen.ListUnresolvedReceiptFindingsRow{}
	txnIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		receipts[r.SubscriptionID] = append(receipts[r.SubscriptionID], r)
		txnIDs = append(txnIDs, r.TransactionID)
	}
	read := map[string]RemoteTransaction{}
	if len(txnIDs) > 0 {
		if reader == nil {
			return res, errors.New("this provider's transactions cannot be read by id")
		}
		if read, err = reader.TransactionsByID(ctx, txnIDs); err != nil {
			return res, fmt.Errorf("read unresolved receipts: %w", err)
		}
	}
	var errs error
	for _, a := range accepted {
		n, err := w.replaySubscriptionReceipts(ctx, a, receipts[billing.SubscriptionID(a.sub.ID).String()], read, now)
		if err == nil {
			err = resolveScheduleFinding(ctx, q, a, n)
		}
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", billing.SubscriptionID(a.sub.ID), err))
			continue
		}
		res.Accepted++
		res.Replayed += n
	}
	return res, errs
}

// replaySubscriptionReceipts places one subscription's receipts oldest first;
// one no accepted bill holds stays a finding and the rest land.
func (w *PGLocalWriter) replaySubscriptionReceipts(ctx context.Context, a acceptedSchedule, rows []gen.ListUnresolvedReceiptFindingsRow, read map[string]RemoteTransaction, now time.Time) (int, error) {
	q := w.DB.Gen(ctx)
	var txns []RemoteTransaction
	for _, r := range rows {
		t, ok := read[r.TransactionID]
		reason := ""
		switch {
		case !ok:
			reason = "the provider reports no readable transaction with this id"
		case t.Type == TransactionTypeVoid:
			reason = "the provider voided this sale"
		}
		if reason != "" {
			if err := recordUnresolvedReceipt(ctx, q, a.sub, &UnresolvedReceiptError{TransactionID: r.TransactionID, Err: errors.New(reason)}); err != nil {
				return 0, err
			}
			continue
		}
		t.SubscriptionID = a.sub.RailSubscriptionID
		if a.entry != nil {
			t.NextBillingAt = a.entry.NextBillingAt
		}
		txns = append(txns, t)
	}
	unresolved := map[string]bool{}
	for pending := txns; ; {
		_, err := backfillSubscriptionPayments(ctx, w.DB, a.sub, pending, now, declaredImportLookback)
		var unplaced *UnresolvedReceiptError
		if !errors.As(err, &unplaced) {
			if err != nil {
				return 0, err
			}
			break
		}
		// backfill kept it as a finding; place the rest.
		unresolved[unplaced.TransactionID] = true
		kept := pending[:0:0]
		for _, t := range pending {
			if t.TransactionID != unplaced.TransactionID {
				kept = append(kept, t)
			}
		}
		if len(kept) == len(pending) {
			return 0, err
		}
		pending = kept
	}
	placed := 0
	for _, r := range rows {
		if t, ok := read[r.TransactionID]; !ok || t.Type == TransactionTypeVoid || unresolved[r.TransactionID] {
			continue
		}
		if _, err := q.MarkReconciliationFindingVanished(ctx, gen.MarkReconciliationFindingVanishedParams{MerchantID: a.sub.MerchantID, ID: r.ID}); err != nil {
			return placed, err
		}
		placed++
	}
	return placed, nil
}

func resolveScheduleFinding(ctx context.Context, q *gen.Queries, a acceptedSchedule, replayed int) error {
	resolution := map[string]any{"schedule_source_reference": a.source, "receipts_placed": replayed}
	if a.entry != nil {
		resolution["provider"] = providerRecordEvidence(a.sub.Rail, a.entry)
	}
	raw, err := json.Marshal(resolution)
	if err != nil {
		return err
	}
	_, err = q.MarkReconciliationFindingAutoFixed(ctx, gen.MarkReconciliationFindingAutoFixedParams{ResolutionEvidence: raw, MerchantID: a.sub.MerchantID, ID: a.finding})
	return err
}
