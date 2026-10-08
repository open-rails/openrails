package nmi

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/shared/progress"
)

// What NMI's transaction history can tell apart (#1114, #1120).
const (
	HistoryVerification    = "verification"
	HistoryOneOffSale      = "one_off_sale"
	HistoryScheduledRebill = "scheduled_rebill"
)

// HistoryNotes are what NMI's history cannot separate; every reader of it
// says so.
var HistoryNotes = []string{
	"scheduled_rebill is NMI's own schedule charge (action source recurring). NMI never retries a declined one, so each is the first attempt of its period.",
	"one_off_sale mixes initial sales, upgrades and retries of declined rebills, whether sent by OpenRails, an earlier billing system or the NMI dashboard. NMI's history cannot tell them apart, so rebill retries are not separated here.",
	"Rates count authorizations, not buyers: a buyer who tries a card three times counts three times.",
}

// HistoryCount is how many authorizations of one kind NMI answered in one
// month with one outcome: category approved, or a refusal's category and
// reason from the one decline classifier.
type HistoryCount struct {
	// Month is the month's first instant, UTC.
	Month            time.Time
	Kind             string
	Category, Reason string
	Count            int
}

// DeclineHistory is the authorizations NMI answered in [Since, Until).
type DeclineHistory struct {
	Since, Until time.Time
	// Counts are ordered by month, kind, category and reason.
	Counts []HistoryCount
	// Undated authorizations carry a date NMI garbled; no month holds them.
	Undated int
}

// DeclineHistory reads the card verifications, one-off sales and scheduled
// rebills NMI answered in [since, until), each counted once in the month it
// was made. It writes nothing. The Query API selects a transaction by its
// last change, so until should be now; it is read one calendar month per
// query, oldest first, in pages of QueryPageLimit. `openrails nmi
// decline-report` and the history job both read through it (#1120).
func (c *NMIClient) DeclineHistory(ctx context.Context, since, until time.Time) (DeclineHistory, error) {
	h := DeclineHistory{Since: since.UTC(), Until: until.UTC()}
	counts := map[HistoryCount]int{}
	seen := map[string]bool{}
	for from := h.Since; from.Before(h.Until); {
		to := MonthStart(from).AddDate(0, 1, 0)
		if to.After(h.Until) {
			to = h.Until
		}
		if err := c.readHistoryWindow(ctx, from, to, &h, counts, seen); err != nil {
			return DeclineHistory{}, err
		}
		from = to
	}
	for k, n := range counts {
		k.Count = n
		h.Counts = append(h.Counts, k)
	}
	slices.SortFunc(h.Counts, func(a, b HistoryCount) int {
		return cmp.Or(a.Month.Compare(b.Month), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Category, b.Category), cmp.Compare(a.Reason, b.Reason))
	})
	return h, nil
}

// readHistoryWindow pages the transactions last changed in [from, to).
func (c *NMIClient) readHistoryWindow(ctx context.Context, from, to time.Time, h *DeclineHistory, counts map[HistoryCount]int, seen map[string]bool) error {
	// end_date is inclusive, to the second.
	filter := QueryFilter{StartDate: from.Format(QueryTimeFormat), EndDate: to.Add(-time.Second).Format(QueryTimeFormat), ResultLimit: QueryPageLimit}
	firsts := map[string]bool{}
	for filter.PageNumber = 0; ; filter.PageNumber++ {
		batch, err := c.TransactionReport(ctx, filter)
		if err != nil {
			return fmt.Errorf("nmi transactions from %s, page %d: %w", from.Format(time.DateOnly), filter.PageNumber, err)
		}
		progress.Mark(ctx, fmt.Sprintf("nmi history %s page %d", from.Format("2006-01"), filter.PageNumber))
		if len(batch.Transactions) > 0 {
			first := strings.TrimSpace(batch.Transactions[0].TransactionID)
			if firsts[first] {
				return fmt.Errorf("nmi transaction pagination repeated page starting at %s", first)
			}
			firsts[first] = true
		}
		for _, t := range batch.Transactions {
			h.count(t, counts, seen)
		}
		if len(batch.Transactions) < QueryPageLimit {
			return nil
		}
	}
}

// count adds a transaction's authorization once, in its month, when it was
// made inside the window.
func (h *DeclineHistory) count(t QueryTransaction, counts map[HistoryCount]int, seen map[string]bool) {
	action, ok := t.Authorization()
	id := strings.TrimSpace(t.TransactionID)
	if !ok || seen[id] {
		return
	}
	seen[id] = true
	at, ok := action.At()
	if !ok {
		h.Undated++
		return
	}
	if at.Before(h.Since) || !at.Before(h.Until) {
		return
	}
	key := HistoryCount{Month: MonthStart(at), Kind: HistoryOneOffSale, Category: string(decline.Approved)}
	switch {
	case action.Is("validate"):
		key.Kind = HistoryVerification
	case strings.EqualFold(strings.TrimSpace(action.Source), "recurring"):
		key.Kind = HistoryScheduledRebill
	}
	if !action.Succeeded() {
		verdict := decline.ClassifyEvidence(t.Evidence(action))
		key.Category, key.Reason = string(verdict.Category), string(verdict.Reason)
	}
	counts[key]++
}

// MonthStart is the first instant of t's calendar month, UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}
