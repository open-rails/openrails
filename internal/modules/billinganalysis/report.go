// Package billinganalysis builds provider-neutral billing evidence reports.
package billinganalysis

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Options selects the evidence window and optional provider.
type Options struct {
	From     time.Time
	To       time.Time
	Provider string
	Location *time.Location
}

// Report is the read-only billing analysis response. Evidence is retained in
// Charges and Failures so an operator can inspect each aggregate without
// querying provider APIs again.
type Report struct {
	From                string              `json:"from"`
	To                  string              `json:"to"`
	Timezone            string              `json:"timezone"`
	Provider            string              `json:"provider,omitempty"`
	Days                []Day               `json:"days"`
	Charges             []Charge            `json:"charges"`
	Failures            []Failure           `json:"failures"`
	CurrentDelinquent   []DelinquentSubject `json:"current_delinquent"`
	EvidenceUnavailable bool                `json:"evidence_unavailable,omitempty"`
}

// Day contains both daily counts and the cumulative open-unbilled roster at
// the end of the local calendar day.
type Day struct {
	Date              string         `json:"date"`
	SettledSignups    int            `json:"settled_signups"`
	SettledRebills    int            `json:"settled_rebills"`
	SettledOther      int            `json:"settled_other"`
	FailedSignups     int            `json:"failed_signups"`
	FailedRebills     int            `json:"failed_rebills"`
	FailedOther       int            `json:"failed_other"`
	OpenUnbilledUsers int            `json:"open_unbilled_users"`
	DelinquentUsers   int            `json:"delinquent_users"`
	Unbilled          []UnbilledCase `json:"unbilled,omitempty"`
}

// Charge is one settled provider event.
type Charge struct {
	Day             string          `json:"day"`
	Kind            string          `json:"kind"`
	Provider        string          `json:"provider"`
	PSPID           string          `json:"psp_id,omitempty"`
	EventKey        string          `json:"event_key"`
	TransactionID   string          `json:"transaction_id,omitempty"`
	SubscriptionRef string          `json:"subscription_ref,omitempty"`
	CustomerRef     string          `json:"customer_ref,omitempty"`
	CustomerEmail   string          `json:"customer_email,omitempty"`
	OrderRef        string          `json:"order_ref,omitempty"`
	AmountCents     int64           `json:"amount_cents,string"`
	Currency        string          `json:"currency,omitempty"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Source          string          `json:"source,omitempty"`
	Raw             json.RawMessage `json:"raw,omitempty"`
}

// Failure is one failed provider charge attempt.
type Failure struct {
	Charge
	DeclineCode   string `json:"decline_code,omitempty"`
	DeclineReason string `json:"decline_reason,omitempty"`
	FailureCount  int    `json:"failure_count"`
}

// UnbilledCase is an open obligation whose provider charge failed and has not
// subsequently succeeded. It is repeated in each later Day snapshot until
// that success closes it.
type UnbilledCase struct {
	ObligationKey   string    `json:"obligation_key"`
	Provider        string    `json:"provider"`
	PSPID           string    `json:"psp_id,omitempty"`
	SubscriptionRef string    `json:"subscription_ref,omitempty"`
	CustomerRef     string    `json:"customer_ref,omitempty"`
	CustomerEmail   string    `json:"customer_email,omitempty"`
	AmountCents     int64     `json:"amount_cents,string"`
	Currency        string    `json:"currency,omitempty"`
	UnbilledSince   string    `json:"unbilled_since"`
	LastFailureAt   time.Time `json:"last_failure_at"`
	FailureCode     string    `json:"failure_code,omitempty"`
	FailureReason   string    `json:"failure_reason,omitempty"`
	FailureCount    int       `json:"failure_count"`
}

// DelinquentSubject is a current provider roster subject observed past_due or
// an open failed obligation.
type DelinquentSubject struct {
	Provider        string     `json:"provider"`
	PSPID           string     `json:"psp_id,omitempty"`
	SubscriptionRef string     `json:"subscription_ref,omitempty"`
	CustomerRef     string     `json:"customer_ref,omitempty"`
	CustomerEmail   string     `json:"customer_email,omitempty"`
	Status          string     `json:"status,omitempty"`
	NextBillingAt   *time.Time `json:"next_billing_at,omitempty"`
	ObligationKey   string     `json:"obligation_key,omitempty"`
}

// FilterDelinquent narrows the current roster for an optional UI status filter.
func FilterDelinquent(items []DelinquentSubject, status string) []DelinquentSubject {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" || status == "all" {
		return items
	}
	out := make([]DelinquentSubject, 0, len(items))
	for _, item := range items {
		if strings.EqualFold(item.Status, status) {
			out = append(out, item)
		}
	}
	return out
}

type evidenceEvent struct {
	Provider, PSPID, EventKey, TransactionID, SubscriptionRef string
	Type, Source, CustomerRef, CustomerEmail, OrderRef        string
	DeclineCode, DeclineReason, Currency                      string
	Success                                                   bool
	AmountCents                                               int64
	OccurredAt                                                time.Time
	Raw                                                       json.RawMessage
}

type subscriptionObservation struct {
	Provider, PSPID, SubscriptionRef, Status, CustomerRef, CustomerEmail string
	NextBillingAt                                                        *time.Time
}

// Build reads retained provider evidence through the caller's merchant-scoped
// DB connection and derives the report entirely locally.
func Build(ctx context.Context, database *db.DB, merchantID merchant.ID, opts Options) (*Report, error) {
	if database == nil {
		return nil, fmt.Errorf("billing analysis database is unavailable")
	}
	if merchantID.IsZero() {
		return nil, fmt.Errorf("billing analysis merchant is required")
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	if opts.To.IsZero() || opts.From.IsZero() {
		return nil, fmt.Errorf("billing analysis time range is required")
	}
	if opts.To.Before(opts.From) {
		return nil, fmt.Errorf("billing analysis end precedes start")
	}
	ctx = merchant.WithID(ctx, merchantID)
	events, err := listEvents(ctx, database, merchantID, opts)
	if err != nil {
		return nil, err
	}
	subs, err := listCurrentSubscriptions(ctx, database, merchantID, opts)
	if err != nil {
		return nil, err
	}
	return derive(events, subs, opts), nil
}

func listEvents(ctx context.Context, database *db.DB, merchantID merchant.ID, opts Options) ([]evidenceEvent, error) {
	var provider *string
	if value := strings.TrimSpace(opts.Provider); value != "" {
		provider = &value
	}
	rows, err := database.Gen(ctx).ListProviderEvidenceTransactions(ctx, gen.ListProviderEvidenceTransactionsParams{Provider: provider, ToAt: &opts.To})
	if err != nil {
		return nil, fmt.Errorf("list billing evidence transactions: %w", err)
	}
	var out []evidenceEvent
	for _, row := range rows {
		currency := ""
		if row.Currency != nil {
			currency = *row.Currency
		}
		out = append(out, evidenceEvent{Provider: row.Provider, PSPID: row.PspID.String(), EventKey: row.EventKey, TransactionID: row.TransactionID, SubscriptionRef: row.SubscriptionRef, Type: row.Type, Success: row.Success, AmountCents: row.AmountCents, Currency: currency, OccurredAt: row.OccurredAt, Source: row.Source, CustomerRef: row.CustomerRef, CustomerEmail: row.CustomerEmail, OrderRef: row.OrderRef, DeclineCode: row.DeclineCode, DeclineReason: row.DeclineReason, Raw: row.Raw})
	}
	return out, nil
}

func listCurrentSubscriptions(ctx context.Context, database *db.DB, merchantID merchant.ID, opts Options) ([]subscriptionObservation, error) {
	var provider *string
	if value := strings.TrimSpace(opts.Provider); value != "" {
		provider = &value
	}
	rows, err := database.Gen(ctx).ListCurrentProviderEvidenceSubscriptions(ctx, gen.ListCurrentProviderEvidenceSubscriptionsParams{Provider: provider, ToAt: opts.To})
	if err != nil {
		return nil, fmt.Errorf("list billing evidence subscriptions: %w", err)
	}
	var out []subscriptionObservation
	for _, row := range rows {
		out = append(out, subscriptionObservation{Provider: row.Provider, PSPID: row.PspID.String(), SubscriptionRef: row.ProviderSubscriptionRef, Status: row.Status, CustomerRef: row.CustomerRef, CustomerEmail: row.CustomerEmail, NextBillingAt: row.NextBillingAt})
	}
	return out, nil
}

func derive(events []evidenceEvent, subs []subscriptionObservation, opts Options) *Report {
	loc := opts.Location
	// Provider queries normally return chronological rows, but derive must remain
	// correct when a caller supplies an unsorted fixture or a future adapter
	// changes query ordering. Stable ordering also makes classification and
	// recovery deterministic when timestamps tie.
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].EventKey < events[j].EventKey
		}
		return events[i].OccurredAt.Before(events[j].OccurredAt)
	})
	classification := make(map[string]classificationState)
	open := make(map[string]*UnbilledCase)
	failureCounts := make(map[string]int)
	charges := make([]Charge, 0)
	failures := make([]Failure, 0)
	byDay := make(map[string]*Day)
	startDay := time.Date(opts.From.In(loc).Year(), opts.From.In(loc).Month(), opts.From.In(loc).Day(), 0, 0, 0, 0, loc)
	endDay := time.Date(opts.To.In(loc).Year(), opts.To.In(loc).Month(), opts.To.In(loc).Day(), 0, 0, 0, 0, loc)
	for day := startDay; !day.After(endDay); day = day.AddDate(0, 0, 1) {
		byDay[day.Format("2006-01-02")] = &Day{Date: day.Format("2006-01-02")}
	}
	for _, e := range events {
		if e.OccurredAt.After(opts.To) {
			continue
		}
		key := obligationKey(e)
		settled := e.Success && strings.EqualFold(e.Type, "sale")
		failed := !e.Success && (strings.EqualFold(e.Type, "sale") || strings.EqualFold(e.Type, "decline"))
		kind := "other"
		if settled || failed {
			state := classification[key]
			kind = classify(e, state)
			if key != "" {
				state.seen = true
				if settled {
					state.successful = true
				}
				classification[key] = state
			}
		}
		day := e.OccurredAt.In(loc).Format("2006-01-02")
		d := byDay[day]
		if settled {
			if d != nil {
				c := chargeFrom(e, kind, day, loc)
				charges = append(charges, c)
				switch kind {
				case "signup":
					d.SettledSignups++
				case "rebill":
					d.SettledRebills++
				default:
					d.SettledOther++
				}
			}
			if key != "" {
				delete(open, key)
				// A successful settlement closes this billing cycle. The next
				// failed attempt should start its own failure count.
				failureCounts[key] = 0
			}
		}
		if failed {
			failureCounts[key]++
			if d != nil {
				f := Failure{Charge: chargeFrom(e, kind, day, loc), DeclineCode: e.DeclineCode, DeclineReason: e.DeclineReason, FailureCount: failureCounts[key]}
				failures = append(failures, f)
				switch kind {
				case "signup":
					d.FailedSignups++
				case "rebill":
					d.FailedRebills++
				default:
					d.FailedOther++
				}
			}
			if key != "" {
				if existing := open[key]; existing != nil {
					existing.LastFailureAt = e.OccurredAt
					existing.FailureCode = e.DeclineCode
					existing.FailureReason = e.DeclineReason
					existing.FailureCount++
				} else {
					open[key] = &UnbilledCase{ObligationKey: key, Provider: e.Provider, PSPID: e.PSPID, SubscriptionRef: e.SubscriptionRef, CustomerRef: e.CustomerRef, CustomerEmail: e.CustomerEmail, AmountCents: e.AmountCents, Currency: e.Currency, UnbilledSince: day, LastFailureAt: e.OccurredAt, FailureCode: e.DeclineCode, FailureReason: e.DeclineReason, FailureCount: 1}
				}
			}
		}
	}
	current := make([]DelinquentSubject, 0)
	rosterKeys := make(map[string]bool)
	for _, s := range subs {
		if strings.EqualFold(s.Status, "past_due") || strings.EqualFold(s.Status, "delinquent") {
			key := s.Provider + "/" + s.PSPID + "/" + s.SubscriptionRef
			rosterKeys[key] = true
			current = append(current, DelinquentSubject{Provider: s.Provider, PSPID: s.PSPID, SubscriptionRef: s.SubscriptionRef, CustomerRef: s.CustomerRef, CustomerEmail: s.CustomerEmail, Status: s.Status, NextBillingAt: s.NextBillingAt, ObligationKey: key})
		}
	}
	for _, c := range open {
		// A current past-due subscription and its latest failed attempt
		// describe the same obligation; expose one roster row.
		if rosterKeys[c.ObligationKey] {
			continue
		}
		current = append(current, DelinquentSubject{Provider: c.Provider, PSPID: c.PSPID, SubscriptionRef: c.SubscriptionRef, CustomerRef: c.CustomerRef, CustomerEmail: c.CustomerEmail, Status: "failed", ObligationKey: c.ObligationKey})
	}
	sort.Slice(current, func(i, j int) bool { return current[i].ObligationKey < current[j].ObligationKey })
	// Re-play events through the window to snapshot the open case set at each
	// day boundary. Events before From establish the baseline roster.
	open = make(map[string]*UnbilledCase)
	failureCounts = make(map[string]int)
	dayEvents := make(map[string][]evidenceEvent)
	for _, e := range events {
		if e.OccurredAt.After(opts.To) {
			continue
		}
		day := e.OccurredAt.In(loc).Format("2006-01-02")
		if e.OccurredAt.Before(opts.From) {
			day = ""
		}
		if day != "" {
			dayEvents[day] = append(dayEvents[day], e)
		}
	}
	apply := func(e evidenceEvent, day string) {
		key := obligationKey(e)
		if e.Success && strings.EqualFold(e.Type, "sale") {
			delete(open, key)
			failureCounts[key] = 0
			return
		}
		if !(!e.Success && (strings.EqualFold(e.Type, "sale") || strings.EqualFold(e.Type, "decline"))) || key == "" {
			return
		}
		failureCounts[key]++
		if c := open[key]; c != nil {
			c.LastFailureAt = e.OccurredAt
			c.FailureCode = e.DeclineCode
			c.FailureReason = e.DeclineReason
			c.FailureCount = failureCounts[key]
		} else {
			open[key] = &UnbilledCase{ObligationKey: key, Provider: e.Provider, PSPID: e.PSPID, SubscriptionRef: e.SubscriptionRef, CustomerRef: e.CustomerRef, CustomerEmail: e.CustomerEmail, AmountCents: e.AmountCents, Currency: e.Currency, UnbilledSince: day, LastFailureAt: e.OccurredAt, FailureCode: e.DeclineCode, FailureReason: e.DeclineReason, FailureCount: 1}
		}
	}
	// Baseline events are applied in chronological order before the first day.
	for _, e := range events {
		if e.OccurredAt.Before(opts.From) {
			apply(e, e.OccurredAt.In(loc).Format("2006-01-02"))
		}
	}
	for day := startDay; !day.After(endDay); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		for _, e := range dayEvents[key] {
			apply(e, key)
		}
		d := byDay[key]
		d.OpenUnbilledUsers = len(open)
		// Historical subscription status is unavailable when the provider only
		// returns the latest roster. Count failed obligations for each day, and
		// merge the point-in-time past-due roster into the report's final day.
		d.DelinquentUsers = len(open)
		if day.Equal(endDay) {
			for obligation := range rosterKeys {
				if _, exists := open[obligation]; !exists {
					d.DelinquentUsers++
				}
			}
		}
		for _, c := range open {
			d.Unbilled = append(d.Unbilled, *c)
		}
		sort.Slice(d.Unbilled, func(i, j int) bool { return d.Unbilled[i].ObligationKey < d.Unbilled[j].ObligationKey })
	}
	// For the current response, snapshots represent the end-of-window state. To
	// avoid exposing a mutable map through every day, JSON values are copies above.
	days := make([]Day, 0, len(byDay))
	for day := startDay; !day.After(endDay); day = day.AddDate(0, 0, 1) {
		days = append(days, *byDay[day.Format("2006-01-02")])
	}
	return &Report{From: opts.From.In(loc).Format(time.RFC3339), To: opts.To.In(loc).Format(time.RFC3339), Timezone: loc.String(), Provider: strings.TrimSpace(opts.Provider), Days: days, Charges: charges, Failures: failures, CurrentDelinquent: current}
}

type classificationState struct {
	seen       bool
	successful bool
}

// classify applies provider source hints without treating every API-originated
// transaction as a new signup or rebill. API sources can include retries or
// later manual charges; once an obligation has settled, those later API events
// remain other unless the provider marks them recurring/renewal.
func classify(e evidenceEvent, state classificationState) string {
	switch strings.ToLower(strings.TrimSpace(e.Source)) {
	case "recurring", "renewal", "rebill":
		return "rebill"
	case "api", "initial", "signup":
		if state.successful {
			// API is not a provider guarantee that this is a renewal. Keep
			// later API-originated events visible as other rather than
			// inflating the rebill total.
			return "other"
		}
		return "signup"
	}
	if e.SubscriptionRef != "" || e.OrderRef != "" {
		if state.seen {
			return "rebill"
		}
		return "signup"
	}
	return "other"
}

func obligationKey(e evidenceEvent) string {
	ref := strings.TrimSpace(e.SubscriptionRef)
	if ref == "" {
		if order := strings.TrimSpace(e.OrderRef); order != "" {
			ref = "order:" + order
		}
	}
	if ref == "" {
		if customer := strings.TrimSpace(e.CustomerRef); customer != "" {
			ref = "customer:" + customer + ":" + e.Currency + ":" + fmt.Sprintf("%d", e.AmountCents)
		}
	}
	if ref == "" {
		if email := strings.TrimSpace(e.CustomerEmail); email != "" {
			ref = "email:" + email + ":" + e.Currency + ":" + fmt.Sprintf("%d", e.AmountCents)
		}
	}
	if ref == "" {
		return ""
	}
	return strings.Join([]string{e.Provider, e.PSPID, ref}, "/")
}
func chargeFrom(e evidenceEvent, kind, day string, loc *time.Location) Charge {
	return Charge{Day: day, Kind: kind, Provider: e.Provider, PSPID: e.PSPID, EventKey: e.EventKey, TransactionID: e.TransactionID, SubscriptionRef: e.SubscriptionRef, CustomerRef: e.CustomerRef, CustomerEmail: e.CustomerEmail, OrderRef: e.OrderRef, AmountCents: e.AmountCents, Currency: e.Currency, OccurredAt: e.OccurredAt, Source: e.Source, Raw: e.Raw}
}
