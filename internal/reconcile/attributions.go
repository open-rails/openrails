package reconcile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// FindingTransactionUnverified is a declared provider charge OpenRails
// recorded no payment for: its id is not the provider's, or the provider's
// record of it is missing, declined or held by no bill.
const FindingTransactionUnverified = "life.import.transaction_unverified"

const (
	// attributionIndexLag: a just-declared id the provider has not indexed
	// is asked for again until this has passed.
	attributionIndexLag = 48 * time.Hour
	attributionBatch    = 1000
)

// RecordUnverifiedTransaction keeps a declared charge as evidence: a finding,
// no payment.
func RecordUnverifiedTransaction(ctx context.Context, q *gen.Queries, merchantID, psp uuid.UUID, transactionID string, subscription *uuid.UUID, reason string) error {
	evidence := map[string]any{"psp_id": billing.PSPID(psp).String(), "transaction_id": transactionID, "reason": reason}
	if subscription != nil {
		evidence["subscription_id"] = billing.SubscriptionID(*subscription).String()
	}
	raw, _ := json.Marshal(evidence)
	action := "No payment was recorded for this declared charge (" + reason + "). Declare the provider's own transaction, or leave it as evidence."
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: merchantID,
		FindingType: FindingTransactionUnverified, SubjectKey: "transaction:" + transactionID, Severity: "medium",
		Status: "requires_review", RecommendedAction: &action, Evidence: raw})
	return err
}

// ChargeAttributions maps those of the PSP's provider transactions a declared
// book attributed to a subscription.
func (l *PGLocalStateLoader) ChargeAttributions(ctx context.Context, pspID uuid.UUID, transactionIDs []string) (map[string]uuid.UUID, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return chargeAttributions(ctx, l.DB.Gen(ctx), mid.UUID(), pspID, transactionIDs)
}

func chargeAttributions(ctx context.Context, q *gen.Queries, merchantID, pspID uuid.UUID, ids []string) (map[string]uuid.UUID, error) {
	out := map[string]uuid.UUID{}
	for len(ids) > 0 {
		n := min(len(ids), attributionBatch)
		rows, err := q.ListChargeAttributionsByTransaction(ctx, gen.ListChargeAttributionsByTransactionParams{MerchantID: merchantID, PspID: pspID, TransactionIds: ids[:n], RowLimit: int32(n)}) // #nosec G115 -- n <= attributionBatch
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.TransactionID] = r.SubscriptionID
		}
		ids = ids[n:]
	}
	return out, nil
}

// AttributionReader reads provider transactions by id.
type AttributionReader interface {
	TransactionReport(ctx context.Context, filter nmi.QueryFilter) (nmi.TransactionReport, error)
}

// AttributionResult counts one merchant's pass.
type AttributionResult struct{ Resolved, Unresolvable, Deferred int }

// ResolveChargeAttributions reads one merchant's due attributions from their
// provider, nmi.MaxQueryIDs per read, and records each approved charge on its
// declared subscription through the same writer a pull uses: settled history
// before the accepted schedule, or the cycle it settles. A missing, declined
// or unplaceable charge records no payment and becomes a finding, as does one
// on a rail OpenRails cannot read by id. A failed read defers the PSP's rows
// with backoff. Only due rows are read, each once, up to limit rows a pass.
func ResolveChargeAttributions(ctx context.Context, database *db.DB, merchantID uuid.UUID, reader func(context.Context, uuid.UUID) (AttributionReader, error), now time.Time, limit int) (AttributionResult, error) {
	var res AttributionResult
	q := database.Gen(ctx)
	psps, err := q.ListPSPsForMerchant(ctx, merchantID)
	if err != nil {
		return res, err
	}
	rails := map[uuid.UUID]string{}
	for _, p := range psps {
		rails[p.ID] = p.Rail
	}
	for read := 0; read < limit; {
		rows, err := q.ListDueChargeAttributions(ctx, gen.ListDueChargeAttributionsParams{MerchantID: merchantID, Now: now, RowLimit: int32(min(attributionBatch, limit-read))}) // #nosec G115 -- at most attributionBatch
		if err != nil || len(rows) == 0 {
			return res, err
		}
		read += len(rows)
		byPSP := map[uuid.UUID][]gen.BillingProviderChargeAttribution{}
		var order []uuid.UUID
		for _, r := range rows {
			if _, ok := byPSP[r.PspID]; !ok {
				order = append(order, r.PspID)
			}
			byPSP[r.PspID] = append(byPSP[r.PspID], r)
		}
		for _, psp := range order {
			pending := byPSP[psp]
			if rail := rails[psp]; rail != string(models.RailNMI) {
				// Only NMI is read by id: another rail's charge stays evidence.
				for _, r := range pending {
					if err := unresolvableAttribution(ctx, q, r, "OpenRails cannot read "+cmp.Or(rail, "this PSP's")+" charges by id", now, &res); err != nil {
						return res, err
					}
				}
				continue
			}
			client, err := reader(ctx, psp)
			if err != nil || client == nil {
				if err := deferAttributions(ctx, q, pending, now.Add(attributionBackoff(pending[0].Attempts)), now, &res); err != nil {
					return res, err
				}
				continue
			}
			for len(pending) > 0 {
				n := min(len(pending), nmi.MaxQueryIDs)
				ids := make([]string, n)
				for i, r := range pending[:n] {
					ids[i] = r.TransactionID
				}
				report, err := client.TransactionReport(ctx, nmi.QueryFilter{TransactionID: strings.Join(ids, ",")})
				if err != nil {
					// The provider refused or failed: leave the rest of this
					// PSP for a later pass.
					if err := deferAttributions(ctx, q, pending, now.Add(attributionBackoff(pending[0].Attempts)), now, &res); err != nil {
						return res, err
					}
					break
				}
				if err := resolveAttributions(ctx, database, pending[:n], report, now, &res); err != nil {
					return res, err
				}
				pending = pending[n:]
			}
		}
	}
	return res, nil
}

func attributionBackoff(attempts int32) time.Duration {
	wait := 15 * time.Minute << min(attempts, 6)
	return min(wait, 24*time.Hour)
}

// unresolvableAttribution closes an attribution that records no payment and
// keeps its charge as a finding.
func unresolvableAttribution(ctx context.Context, q *gen.Queries, r gen.BillingProviderChargeAttribution, reason string, now time.Time, res *AttributionResult) error {
	if _, err := q.MarkChargeAttributionUnresolvable(ctx, gen.MarkChargeAttributionUnresolvableParams{MerchantID: r.MerchantID, PspID: r.PspID, TransactionID: r.TransactionID, Reason: reason, Now: now}); err != nil {
		return err
	}
	res.Unresolvable++
	return RecordUnverifiedTransaction(ctx, q, r.MerchantID, r.PspID, r.TransactionID, &r.SubscriptionID, reason)
}

func deferAttributions(ctx context.Context, q *gen.Queries, rows []gen.BillingProviderChargeAttribution, next, now time.Time, res *AttributionResult) error {
	for _, r := range rows {
		if _, err := q.DeferChargeAttribution(ctx, gen.DeferChargeAttributionParams{MerchantID: r.MerchantID, PspID: r.PspID, TransactionID: r.TransactionID, NextAttemptAt: next, Now: now}); err != nil {
			return err
		}
		res.Deferred++
	}
	return nil
}

// resolveAttributions records one read's transactions.
func resolveAttributions(ctx context.Context, database *db.DB, rows []gen.BillingProviderChargeAttribution, report nmi.TransactionReport, now time.Time, res *AttributionResult) error {
	found := map[string]nmi.QueryTransaction{}
	for _, t := range report.Transactions {
		found[strings.TrimSpace(t.TransactionID)] = t
	}
	q := database.Gen(ctx)
	for _, r := range rows {
		unresolvable := func(reason string) error { return unresolvableAttribution(ctx, q, r, reason, now, res) }
		t, ok := found[r.TransactionID]
		if !ok {
			if r.DeclaredAt != nil && now.Sub(*r.DeclaredAt) < attributionIndexLag {
				if err := deferAttributions(ctx, q, []gen.BillingProviderChargeAttribution{r}, now.Add(time.Hour), now, res); err != nil {
					return err
				}
				continue
			}
			if err := unresolvable("the provider has no transaction with this id"); err != nil {
				return err
			}
			continue
		}
		if err := qualifyNMITransaction(t); err != nil {
			if err := unresolvable("the provider's record is unreadable: " + err.Error()); err != nil {
				return err
			}
			continue
		}
		charge := chargeAction(t)
		reason := ""
		switch {
		case charge == nil:
			reason = "the provider's transaction is not a sale"
		case charge.Type == TransactionTypeVoid:
			reason = "the provider voided this sale"
		case !charge.Success:
			reason = "the provider declined this charge"
		case !charge.positive():
			reason = "the provider's sale has no amount"
		}
		if reason != "" {
			if err := unresolvable(reason); err != nil {
				return err
			}
			continue
		}
		sub, err := subscriptions.NewSubscriptionRepo(database).GetByID(ctx, r.SubscriptionID)
		if err != nil {
			return err
		}
		if sub.PspID != r.PspID || sub.CustomerID != r.CustomerID {
			if err := unresolvable("the declared subscription belongs to another account"); err != nil {
				return err
			}
			continue
		}
		charge.SubscriptionID = sub.RailSubscriptionID
		_, err = backfillSubscriptionPayments(ctx, database, sub, []RemoteTransaction{*charge}, now, declaredImportLookback)
		var unplaced *UnresolvedReceiptError
		if errors.As(err, &unplaced) {
			if err := unresolvable("no accepted bill holds it: " + unplaced.Err.Error()); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("record attributed charge %s: %w", r.TransactionID, err)
		}
		rail := string(sub.Rail)
		payment, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: r.MerchantID, PspID: &r.PspID, Channel: string(models.ChannelRail), Rail: &rail, TransactionID: r.TransactionID})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := unresolvable("no payment could be recorded for it"); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if _, err := q.ResolveChargeAttribution(ctx, gen.ResolveChargeAttributionParams{MerchantID: r.MerchantID, PspID: r.PspID, TransactionID: r.TransactionID, PaymentID: payment.ID, Now: now}); err != nil {
				return err
			}
			res.Resolved++
		}
	}
	return nil
}
