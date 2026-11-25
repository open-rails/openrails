package reconcile

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// CCBillFetcher pulls CCBill via DataLink batch exports: the ACTIVEMEMBERS
// roster, and REBILL/CANCELLATION/EXPIRE/REFUND/CHARGEBACK over [Since,Until),
// a day per export. A live account is Paced: one read is the roster (an empty
// window) or one day of transactions (TransactionsOnly).
// It has no vault read. A termination appears only inside its window, so
// absence means inactive-or-out-of-window, never unknown-to-CCBill. Rows carry
// no currency: the matched local record denominates them, never a default. The
// roster has no server-side filter, so SubscriptionID narrows here.
type CCBillFetcher struct {
	DataLink *ccbill.DataLinkClient
}

// NewCCBillFetcher builds a fetcher over an existing DataLink client.
func NewCCBillFetcher(dl *ccbill.DataLinkClient) *CCBillFetcher {
	return &CCBillFetcher{DataLink: dl}
}

func (f *CCBillFetcher) Name() string { return string(ProviderCCBill) }

// Paced reports whether the account's DataLink answers once an hour.
func (f *CCBillFetcher) Paced() bool { return f.DataLink.Paced() }

func (f *CCBillFetcher) Capabilities() Capabilities {
	return Capabilities{
		Subscriptions: true,
		Transactions:  true,
		Refunds:       true,
		Chargebacks:   true,
		Vault:         false,
	}
}

func (f *CCBillFetcher) Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error) {
	observedAt := fetchObservedAt(params)
	snap := &RemoteSnapshot{
		Provider:     ProviderCCBill,
		FetchedAt:    observedAt,
		Capabilities: f.Capabilities(),
	}

	// Transaction-type exports need a window; default to the trailing 30 days
	// when the caller didn't bound the fetch.
	until := params.Until
	if until.IsZero() {
		until = observedAt
	}
	since := params.Since
	if since.IsZero() {
		since = until.Add(-30 * 24 * time.Hour)
	}
	if f.Paced() && !params.TransactionsOnly && since.Before(until) {
		return nil, fmt.Errorf("ccbill: a live account's DataLink answers once an hour, so its roster and its transactions are separate reads")
	}

	if !params.TransactionsOnly {
		active, err := f.DataLink.FetchActiveMembers(ctx)
		if err != nil {
			return nil, fmt.Errorf("ccbill activemembers export: %w", err)
		}
		for _, rec := range active {
			if params.SubscriptionID != "" && !rails.SameSubscription(string(ProviderCCBill), rec.SubscriptionID, params.SubscriptionID) {
				continue
			}
			snap.Subscriptions = append(snap.Subscriptions, CCBillRemoteSubscription(rec))
		}
	}

	rows, err := f.DataLink.FetchTransactionExport(ctx, since, until, ccbill.AllDataLinkTxnTypes)
	if err != nil {
		return nil, fmt.Errorf("ccbill transaction export: %w", err)
	}
	for _, row := range rows {
		if params.SubscriptionID != "" && !rails.SameSubscription(string(ProviderCCBill), row.SubscriptionID(), params.SubscriptionID) {
			continue
		}
		switch row.TransactionType {
		case ccbill.DataLinkTxnCancellation, ccbill.DataLinkTxnExpire:
			snap.Subscriptions = append(snap.Subscriptions, normalizeCCBillTermination(row))
		case ccbill.DataLinkTxnRebill, ccbill.DataLinkTxnRefund, ccbill.DataLinkTxnChargeback:
			snap.Transactions = append(snap.Transactions, CCBillRemoteTransaction(row))
		default:
			// Unknown events remain raw evidence, not evidence of a failed charge.
			snap.Transactions = append(snap.Transactions, RemoteTransaction{
				SubscriptionID: row.SubscriptionID(),
				Type:           TransactionTypeUnknown,
				Success:        false,
				Raw:            ccbillRowRaw(row),
			})
		}
	}

	return snap, nil
}

// ccbillTransactionKey ignores leading zeros: a CCBill id is a number.
func ccbillTransactionKey(id string) string { return strings.TrimLeft(strings.TrimSpace(id), "0") }

// Callback rebills are checked once DataLink has had this long to list them,
// and for this long after.
const (
	ccbillListingGrace  = 36 * time.Hour
	ccbillListingWindow = 30 * 24 * time.Hour
)

// confirmCCBillCallbackRebills records the rebills snap lists, resolves the
// unconfirmed-charge findings they confirm, and answers a finding for each
// callback rebill received between 30 days and 36 hours ago, before the read
// reached 36 hours past it, that no read has listed. It only reports: CCBill
// signs nothing, and an unlisted rebill is a question for a person.
func (e *Engine) confirmCCBillCallbackRebills(ctx context.Context, pspID uuid.UUID, snap *RemoteSnapshot, readThrough, now time.Time) ([]Finding, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	q := e.Local.DB.Gen(ctx)
	var keys []string
	for _, t := range snap.Transactions {
		if key := ccbillTransactionKey(t.TransactionID); t.Type == TransactionTypeSale && key != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) > 0 {
		if err := q.RecordProviderListedRebills(ctx, gen.RecordProviderListedRebillsParams{MerchantID: mid.UUID(), PspID: pspID, TransactionKeys: keys, ListedAt: now}); err != nil {
			return nil, fmt.Errorf("record listed rebills: %w", err)
		}
	}
	confirmed, err := q.ReconcileListConfirmedUnconfirmedCharges(ctx, gen.ReconcileListConfirmedUnconfirmedChargesParams{MerchantID: mid.UUID(), PspID: pspID})
	if err != nil {
		return nil, err
	}
	for _, id := range confirmed {
		if err := e.Store.MarkFindingVanished(ctx, id); err != nil {
			return nil, fmt.Errorf("resolve confirmed charge %s: %w", id, err)
		}
	}
	if readThrough.IsZero() || readThrough.After(now) {
		readThrough = now
	}
	rows, err := q.ReconcileListUnconfirmedCallbackRebills(ctx, gen.ReconcileListUnconfirmedCallbackRebillsParams{MerchantID: mid.UUID(), PspID: pspID,
		ReceivedFrom: now.Add(-ccbillListingWindow), ReceivedBefore: readThrough.Add(-ccbillListingGrace)})
	if err != nil {
		return nil, err
	}
	out := make([]Finding, 0, len(rows))
	for _, p := range rows {
		local := map[string]any{"payment_id": p.ID.String(), "customer_id": p.CustomerID.String(), "transaction_id": p.TransactionID,
			"amount": strconv.FormatInt(p.Amount, 10), "currency": p.Currency, "received_at": p.CreatedAt.UTC(), "callback_timestamp": p.PurchasedAt.UTC()}
		if p.SubscriptionID != nil {
			local["subscription_id"] = p.SubscriptionID.String()
		}
		if p.RailSubscriptionID != nil {
			local["rail_subscription_id"] = *p.RailSubscriptionID
		}
		out = append(out, Finding{
			Provider:          ProviderCCBill,
			Type:              FindingChargeUnconfirmed,
			SubjectKey:        p.TransactionID,
			Severity:          SeverityCritical,
			Status:            FindingStatusRequiresReview,
			RequiresAdmin:     true,
			RecommendedAction: "no CCBill DataLink read lists this rebill, recorded from an unsigned callback; check the transaction, its subscription and amount in CCBill's admin before trusting the payment and the period it paid for",
			LocalEvidence:     local,
			RemoteEvidence:    map[string]any{"source": "ccbill_datalink_export", "read_through": readThrough.UTC()},
		})
	}
	return out, nil
}

// ccbillLapseInDoubt reports a CCBill lapse the roster contradicts: it cut
// paid access short, or the roster still lists the member 48 hours after it.
func ccbillLapseInDoubt(provider Provider, s *LocalSubscription, now time.Time) bool {
	if provider != ProviderCCBill || s.Status != string(models.StatusCanceled) || s.CancelType != string(models.CancelTypeExpired) || s.EndedAt == nil {
		return false
	}
	cutShort := s.CurrentPeriodEndsAt != nil && s.EndedAt.Before(*s.CurrentPeriodEndsAt)
	return cutShort || now.Sub(*s.EndedAt) > 48*time.Hour
}

// CCBillRemoteSubscription is one ACTIVEMEMBERS row as a pull reads it.
func CCBillRemoteSubscription(rec ccbill.CCBillRecord) RemoteSubscription {
	sub := RemoteSubscription{
		RailSubscriptionID: rec.SubscriptionID,
		Status:             SubscriptionStatusActive,
		RawStatus:          strings.TrimSpace(rec.Status),
		Email:              strings.TrimSpace(rec.Email),
		Username:           strings.TrimSpace(rec.Username),
		PlanID:             strings.TrimSpace(rec.Field2),
		Raw: rawJSON(map[string]any{
			"source": "ccbill_activemembers",
			"record": rec,
		}),
	}
	if !ccbill.IsDataLinkActiveStatus(rec.Status) {
		sub.Status = SubscriptionStatusUnknown
	}
	if next, err := ccbill.ParseTime(rec.RebillDate); err == nil {
		sub.NextBillingAt = &next
	}
	return sub
}

// normalizeCCBillTermination is a CANCELLATION or EXPIRE export row. A
// cancellation only stops rebilling: the member keeps the time paid for,
// through its expire date (NextBillingAt, as a status read names it), so it
// never ends access now. An expiry does.
func normalizeCCBillTermination(row ccbill.DataLinkExportRow) RemoteSubscription {
	sub := RemoteSubscription{
		RailSubscriptionID: row.SubscriptionID(),
		Status:             SubscriptionStatusCanceled,
		RawStatus:          string(row.TransactionType),
		Raw:                ccbillRowRaw(row),
	}
	if row.TransactionType == ccbill.DataLinkTxnExpire {
		sub.Status = SubscriptionStatusExpired
		return sub
	}
	if expires, err := ccbill.ParseDate(row.ExpireDate()); err == nil {
		paid := ccbill.DayEnd(expires)
		sub.NextBillingAt = &paid
	} else if expires, err := ccbill.ParseInstant(row.ExpireDate()); err == nil {
		sub.NextBillingAt = &expires
	}
	return sub
}

// ccbillStatusNotRebilling is a status read's subscriptionStatus for an
// active subscription with no further rebill.
const ccbillStatusNotRebilling = "1"

// stoppedRebillingOnly reports a canceled remote whose provider only stopped
// rebilling, its member keeping the time paid for: a CCBill CANCELLATION row,
// or a CCBill status read answering 1.
func stoppedRebillingOnly(provider Provider, r *RemoteSubscription) bool {
	return provider == ProviderCCBill && r != nil && r.Status == SubscriptionStatusCanceled &&
		(r.RawStatus == string(ccbill.DataLinkTxnCancellation) || r.RawStatus == ccbillStatusNotRebilling)
}

// unreversedSales is txns less each sale a refund or chargeback among them
// reverses. CCBill's rows name no transaction, so a reversal takes the
// latest sale before it of its amount, or of any amount when it names none.
func unreversedSales(txns []RemoteTransaction) []RemoteTransaction {
	reversed := map[int]bool{}
	for _, r := range txns {
		if !r.Success || r.Type != TransactionTypeRefund && r.Type != TransactionTypeChargeback {
			continue
		}
		match := -1
		for i, s := range txns {
			if reversed[i] || !s.Success || s.Type != TransactionTypeSale && s.Type != TransactionTypeSettlement ||
				s.OccurredAt.After(r.OccurredAt) || !sameAmount(s, r) {
				continue
			}
			if match < 0 || s.OccurredAt.After(txns[match].OccurredAt) {
				match = i
			}
		}
		if match >= 0 {
			reversed[match] = true
		}
	}
	out := make([]RemoteTransaction, 0, len(txns))
	for i, t := range txns {
		if !reversed[i] {
			out = append(out, t)
		}
	}
	return out
}

// sameAmount reports whether a reversal is of the sale's whole amount;
// true when either names none.
func sameAmount(sale, reversal RemoteTransaction) bool {
	a, okA := new(big.Rat).SetString(strings.TrimSpace(sale.Amount))
	b, okB := new(big.Rat).SetString(strings.TrimSpace(reversal.Amount))
	switch {
	case okA && okB:
		return a.Abs(a).Cmp(b.Abs(b)) == 0
	case sale.AmountCents != 0 && reversal.AmountCents != 0:
		return abs(sale.AmountCents) == abs(reversal.AmountCents)
	}
	return true
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// renewedSince reports a successful sale at or after cutoff.
func renewedSince(txns []RemoteTransaction, cutoff time.Time) bool {
	for _, t := range txns {
		if (t.Type == TransactionTypeSale || t.Type == TransactionTypeSettlement) && t.Success && !t.OccurredAt.Before(cutoff) {
			return true
		}
	}
	return false
}

// CCBillRemoteTransaction is one rebill, refund or chargeback export row.
func CCBillRemoteTransaction(row ccbill.DataLinkExportRow) RemoteTransaction {
	txn := RemoteTransaction{
		TransactionID:  row.TransactionID(),
		SubscriptionID: row.SubscriptionID(),
		Success:        true,
		Raw:            ccbillRowRaw(row),
	}
	switch row.TransactionType {
	case ccbill.DataLinkTxnRefund:
		txn.Type = TransactionTypeRefund
	case ccbill.DataLinkTxnChargeback:
		txn.Type = TransactionTypeChargeback
	default:
		txn.Type = TransactionTypeSale
	}
	txn.setAmount(row.Amount())
	if ts, err := ccbill.ParseTime(row.Timestamp()); err == nil {
		txn.OccurredAt = ts
	}
	return txn
}

func ccbillRowRaw(row ccbill.DataLinkExportRow) []byte {
	return rawJSON(map[string]any{
		"source": "ccbill_datalink_export",
		"type":   string(row.TransactionType),
		"fields": row.Fields,
	})
}
