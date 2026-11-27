package pspimport

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
)

// inferredPrefix names a schedule an import read from its sales: no NMI id
// is spelled so, so nothing at NMI is ever looked up or acted on by it.
const inferredPrefix = "inferred:"

// inferred reports a staged subscription an import read from its sales.
func inferred(ref string) bool { return strings.HasPrefix(ref, inferredPrefix) }

// inferredSale is one approved recurring sale no listed schedule claims.
type inferredSale struct {
	record gen.BillingPspImportRecord
	data   transactionData
	facts  facts
}

// inferSchedules stages the NMI schedules a pull's history holds but NMI no
// longer lists: NMI lists only live schedules and its reports name none, so a
// run of approved recurring sales on one vault, order and amount that no
// listed schedule claims, and OpenRails holds none of, is the closest fact of
// a deleted one. A run breaks where a gap passes half again its cadence (the
// median gap). Each lands ended only, with its sales as its payments; a run
// no provider price prices stays history without a subscription.
func (im *Importer) inferSchedules(ctx context.Context, row gen.BillingPspImport) error {
	var candidates []inferredSale
	if err := im.eachRecord(ctx, row, billing.PSPImportTransaction, func(r gen.BillingPspImportRecord) error {
		if r.SubscriptionRef != nil || r.CustomerRef == nil {
			return nil
		}
		var data transactionData
		if err := json.Unmarshal(r.Data, &data); err != nil {
			return err
		}
		if !data.Recurring || !data.Success || data.Type != "sale" || data.Amount <= 0 {
			return nil
		}
		var f facts
		if err := json.Unmarshal(r.Facts, &f); err != nil {
			return err
		}
		candidates = append(candidates, inferredSale{record: r, data: data, facts: f})
		return nil
	}); err != nil {
		return err
	}
	// A sale OpenRails holds is of a schedule it knows: nothing to infer.
	held := map[string]bool{}
	for start := 0; start < len(candidates); start += stageBatch {
		chunk := candidates[start:min(start+stageBatch, len(candidates))]
		ids := make([]string, 0, len(chunk))
		for _, c := range chunk {
			ids = append(ids, c.data.TransactionID)
		}
		rows, err := im.DB.Gen(ctx).ListPSPHeldTransactions(ctx, gen.ListPSPHeldTransactionsParams{MerchantID: row.MerchantID, PspID: row.PspID, Refs: ids})
		if err != nil {
			return err
		}
		for _, h := range rows {
			held[h.Ref] = true
		}
	}
	groups := map[string][]inferredSale{}
	for _, c := range candidates {
		if held[c.data.TransactionID] {
			continue
		}
		order := ""
		if ids := c.facts[FactOrderID]; len(ids) > 0 {
			order = ids[0]
		}
		key := strings.Join([]string{*c.record.CustomerRef, order, c.data.Currency, strconv.FormatInt(c.data.Amount, 10)}, "\x1f")
		groups[key] = append(groups[key], c)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var recs []staged
	for _, key := range keys {
		sales := groups[key]
		sort.Slice(sales, func(i, j int) bool {
			a, b := sales[i].data, sales[j].data
			return a.OccurredAt.Before(b.OccurredAt) || a.OccurredAt.Equal(b.OccurredAt) && a.TransactionID < b.TransactionID
		})
		var gaps []int
		for i := 1; i < len(sales); i++ {
			gaps = append(gaps, int(calendarDay(sales[i].data.OccurredAt).Sub(calendarDay(sales[i-1].data.OccurredAt))/(24*time.Hour)))
		}
		cadence := 0
		if len(gaps) > 0 {
			sorted := slices.Clone(gaps)
			slices.Sort(sorted)
			cadence = sorted[len(sorted)/2]
		}
		for start := 0; start < len(sales); {
			end := start + 1
			for end < len(sales) && (cadence == 0 || gaps[end-1]*2 <= cadence*3) {
				end++
			}
			recs = append(recs, inferredRun(sales[start:end], cadence)...)
			start = end
		}
	}
	return im.stage(ctx, row, recs)
}

// inferredRun is one run's schedule and its sales placed on it.
func inferredRun(run []inferredSale, cadence int) []staged {
	first, last := run[0], run[len(run)-1]
	ref := inferredPrefix + first.data.TransactionID
	vault := deref(first.record.CustomerRef)
	ids := make([]string, 0, len(run))
	for _, s := range run {
		ids = append(ids, s.data.TransactionID)
	}
	f := facts{}
	f.add(FactSubscriptionRef, ref)
	f.add(FactCustomerRef, vault)
	f.add(FactOrderID, first.facts[FactOrderID]...)
	paid := last.data.OccurredAt
	data := subscriptionData{Amount: first.data.Amount, Currency: first.data.Currency, IntervalHours: cadence * 24, StartedAt: first.data.OccurredAt,
		Ended: true, EndKind: string(reconcile.DeclaredCancelProviderTerminated), Inferred: true, LastPaidAt: &paid}
	data.Evidence, _ = json.Marshal(map[string]any{"source": "nmi_psp_import", "inferred_from": ids})
	out := []staged{{kind: billing.PSPImportSubscription, ref: ref, customerRef: vault, occurredAt: first.data.OccurredAt, facts: f, data: data}}
	for _, s := range run {
		placed := facts{}
		for name, values := range s.facts {
			placed.add(name, values...)
		}
		placed.add(FactSubscriptionRef, ref)
		out = append(out, staged{kind: billing.PSPImportTransaction, ref: s.record.Ref, customerRef: vault, subscriptionRef: ref, occurredAt: s.data.OccurredAt,
			facts: placed, data: json.RawMessage(s.record.Data)})
	}
	return out
}
