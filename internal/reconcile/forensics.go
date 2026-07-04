package reconcile

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// DunningForensics answers, per provider: did dunning ever run, when did it
// stop, and did attempts fail? For every locally past_due or
// cancelled-as-expired subscription it lines up THREE evidence sources
// (decision 2026-06-11 — forensics are generic OpenRails functionality):
//
//  1. "provider" — the rail's own charge-attempt timeline (declines
//     included), from the fetcher's transaction snapshot;
//  2. "local"    — the local retry fields (last_retry_at / retry_attempts /
//     next_retry_at), imported from legacy by the migration;
//  3. "history"  — Postgres history (#735): the imported legacy
//     rebill/scheduler history (doujins #387) plus failed-payment rows —
//     deep history the provider APIs cannot return.
type DunningForensics struct {
	Provider              Provider `json:"provider"`
	SubscriptionsExamined int      `json:"subscriptions_examined"`
	// NeverAttempted: the rail recorded declines but local dunning never
	// acted (retry fields frozen/null) — "dunning never ran".
	NeverAttempted int `json:"never_attempted"`
	// AttemptedExhausted: local dunning acted and gave up (attempts > 0, no
	// next retry scheduled or the subscription is already terminal).
	AttemptedExhausted int `json:"attempted_and_exhausted"`
	// AttemptedInProgress: local dunning acted and still has a retry queued.
	AttemptedInProgress int `json:"attempted_in_progress"`
	// NoRemoteDeclines: neither the rail nor local dunning shows
	// attempts in the examined window.
	NoRemoteDeclines int `json:"no_remote_declines"`
	// LastLocalDunningAction is the newest last_retry_at across the examined
	// set — when local dunning last did anything at all.
	LastLocalDunningAction *time.Time `json:"last_local_dunning_action,omitempty"`
	// LastProviderAttempt is the newest provider-side charge attempt
	// (success or decline) across the examined set.
	LastProviderAttempt *time.Time `json:"last_provider_attempt,omitempty"`
	// LastHistoryAttempt is the newest charge-type history event across the
	// examined set (incl. imported legacy rebill attempts).
	LastHistoryAttempt *time.Time `json:"last_history_attempt,omitempty"`
	// LastDunningActionAnySource is the max of the three, with the source
	// that supplied it — "when did dunning last act, per ANY evidence".
	LastDunningActionAnySource *time.Time     `json:"last_dunning_action_any_source,omitempty"`
	LastDunningActionVia       string         `json:"last_dunning_action_via,omitempty"` // provider | local | history
	DeclineReasons             map[string]int `json:"decline_reason_histogram,omitempty"`
	// HistorySource documents the third evidence source's availability:
	// "ok: N events (M correlated)", "not configured", or "unavailable: ...".
	// An unreachable source degrades to this note, never an error.
	HistorySource string `json:"history_source,omitempty"`
	// Details are per-subscription lines, capped at forensicsDetailCap.
	Details        []DunningSubscriptionReport `json:"details,omitempty"`
	DetailsCovered int                         `json:"details_covered"`
}

// TimelineEvent is one merged-timeline entry, tagged by evidence source.
type TimelineEvent struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // provider | local | history
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

// DunningSubscriptionReport is one subscription's cross-referenced timeline.
type DunningSubscriptionReport struct {
	SubscriptionID     string     `json:"subscription_id"`
	RailSubscriptionID string     `json:"rail_subscription_id,omitempty"`
	Status             string     `json:"status"`
	CancelType         string     `json:"cancel_type,omitempty"`
	RetryAttempts      int        `json:"retry_attempts"`
	LastRetryAt        *time.Time `json:"last_retry_at,omitempty"`
	NextRetryAt        *time.Time `json:"next_retry_at,omitempty"`
	RemoteDeclines     int        `json:"remote_declines"`
	RemoteSuccesses    int        `json:"remote_successes"`
	FirstDeclineAt     *time.Time `json:"first_decline_at,omitempty"`
	LastDeclineAt      *time.Time `json:"last_decline_at,omitempty"`
	DeclineReasons     []string   `json:"decline_reasons,omitempty"`
	// History (Postgres history incl. imported legacy):
	HistoryEvents    int        `json:"history_events,omitempty"`
	HistoryFailures  int        `json:"history_failures,omitempty"`
	HistorySuccesses int        `json:"history_successes,omitempty"`
	FirstHistoryAt   *time.Time `json:"first_history_at,omitempty"`
	LastHistoryAt    *time.Time `json:"last_history_at,omitempty"`
	Classification   string     `json:"classification"`
	// Timeline merges the three sources chronologically (most recent
	// timelineCap events kept), each entry tagged provider|local|history.
	Timeline []TimelineEvent `json:"timeline,omitempty"`
}

const (
	forensicsDetailCap = 200
	timelineCap        = 30
)

// historyEventClass classifies an analytics event for forensics counting:
// charge attempt (success/failure) vs other lifecycle noise.
func historyEventClass(eventType, status string) (isAttempt, isFailure, isSuccess bool) {
	et := strings.ToLower(eventType)
	st := strings.ToLower(status)
	switch {
	case strings.Contains(et, "charge") || strings.Contains(et, "rebill") || et == "renewed":
		fail := strings.Contains(et, "fail") || strings.Contains(et, "declin") ||
			strings.Contains(st, "fail") || strings.Contains(st, "declin")
		return true, fail, !fail
	default:
		return false, false, false
	}
}

// computeDunningForensics builds the per-provider dunning report from the
// snapshot's transaction timeline, the local retry fields, and the analytics
// history events. historyNote documents the history source's availability.
func computeDunningForensics(provider Provider, snap *RemoteSnapshot, local *LocalState, history []HistoryEvent, historyNote string, now time.Time) *DunningForensics {
	idx := buildLocalIndex(local)
	ridx := buildRemoteIndex(snap)
	corr := &correlator{local: idx, remote: ridx}

	// Correlate the remote charge timeline to local subscriptions once.
	type timeline struct {
		declines, successes           int
		firstDeclineAt, lastDeclineAt *time.Time
		lastAttemptAt                 *time.Time
		reasons                       map[string]int
		// history (Postgres) evidence:
		histEvents, histFailures, histSuccesses int
		firstHistAt, lastHistAt                 *time.Time
		lastHistAttemptAt                       *time.Time
		events                                  []TimelineEvent
	}
	timelines := map[string]*timeline{} // keyed by local subscription id
	tlFor := func(subID string) *timeline {
		tl := timelines[subID]
		if tl == nil {
			tl = &timeline{reasons: map[string]int{}}
			timelines[subID] = tl
		}
		return tl
	}

	for i := range snap.Transactions {
		t := &snap.Transactions[i]
		if t.Type != TransactionTypeSale && t.Type != TransactionTypeDecline {
			continue
		}
		sub, _, _ := corr.subForTxn(t)
		if sub == nil {
			continue
		}
		tl := tlFor(sub.ID.String())
		at := t.OccurredAt.UTC()
		if !t.OccurredAt.IsZero() {
			if tl.lastAttemptAt == nil || at.After(*tl.lastAttemptAt) {
				tl.lastAttemptAt = &at
			}
		}
		if t.Success {
			tl.successes++
			if !t.OccurredAt.IsZero() {
				tl.events = append(tl.events, TimelineEvent{
					At: at, Source: "provider", Kind: "charge_success",
					Detail: fmt.Sprintf("%s %d %s", t.TransactionID, t.AmountCents, t.Currency),
				})
			}
			continue
		}
		tl.declines++
		if !t.OccurredAt.IsZero() {
			if tl.firstDeclineAt == nil || at.Before(*tl.firstDeclineAt) {
				tl.firstDeclineAt = &at
			}
			if tl.lastDeclineAt == nil || at.After(*tl.lastDeclineAt) {
				tl.lastDeclineAt = &at
			}
			tl.events = append(tl.events, TimelineEvent{
				At: at, Source: "provider", Kind: "charge_declined", Detail: t.DeclineReason,
			})
		}
		reason := t.DeclineReason
		if reason == "" {
			reason = "(no decline reason)"
		}
		tl.reasons[reason]++
	}

	// Third source: analytics history events, correlated by local
	// subscription id first, rail subscription id second.
	historyCorrelated := 0
	for i := range history {
		ev := &history[i]
		var sub *LocalSubscription
		if ev.SubscriptionID != nil {
			sub = idx.byID[*ev.SubscriptionID]
		}
		if sub == nil && ev.RailSubscriptionID != "" {
			sub = idx.byPSID[ev.RailSubscriptionID]
		}
		if sub == nil {
			continue
		}
		historyCorrelated++
		tl := tlFor(sub.ID.String())
		at := ev.OccurredAt.UTC()
		tl.histEvents++
		if tl.firstHistAt == nil || at.Before(*tl.firstHistAt) {
			tl.firstHistAt = &at
		}
		if tl.lastHistAt == nil || at.After(*tl.lastHistAt) {
			tl.lastHistAt = &at
		}
		isAttempt, isFailure, isSuccess := historyEventClass(ev.EventType, ev.Status)
		if isFailure {
			tl.histFailures++
		}
		if isAttempt && isSuccess {
			tl.histSuccesses++
		}
		if isAttempt && (tl.lastHistAttemptAt == nil || at.After(*tl.lastHistAttemptAt)) {
			tl.lastHistAttemptAt = &at
		}
		detail := ev.Status
		if ev.RailTransactionID != "" {
			if detail != "" {
				detail += " "
			}
			detail += ev.RailTransactionID
		}
		tl.events = append(tl.events, TimelineEvent{
			At: at, Source: "history", Kind: ev.EventType, Detail: strings.TrimSpace(detail),
		})
	}

	report := &DunningForensics{
		Provider:       provider,
		DeclineReasons: map[string]int{},
		HistorySource:  historyNote,
	}
	if historyNote == "" && len(history) > 0 {
		report.HistorySource = fmt.Sprintf("ok: %d events (%d correlated)", len(history), historyCorrelated)
	} else if strings.HasPrefix(historyNote, "ok") {
		report.HistorySource = fmt.Sprintf("%s (%d correlated)", historyNote, historyCorrelated)
	}

	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		examined := s.Status == "past_due" || (s.Status == "cancelled" && s.CancelType == "expired")
		if !examined {
			continue
		}
		report.SubscriptionsExamined++

		tl := timelines[s.ID.String()]
		line := DunningSubscriptionReport{
			SubscriptionID:     s.ID.String(),
			RailSubscriptionID: s.RailSubscriptionID,
			Status:             s.Status,
			CancelType:         s.CancelType,
			RetryAttempts:      s.RetryAttempts,
			LastRetryAt:        s.LastRetryAt,
			NextRetryAt:        s.NextRetryAt,
		}
		var events []TimelineEvent
		if tl != nil {
			line.RemoteDeclines = tl.declines
			line.RemoteSuccesses = tl.successes
			line.FirstDeclineAt = tl.firstDeclineAt
			line.LastDeclineAt = tl.lastDeclineAt
			line.HistoryEvents = tl.histEvents
			line.HistoryFailures = tl.histFailures
			line.HistorySuccesses = tl.histSuccesses
			line.FirstHistoryAt = tl.firstHistAt
			line.LastHistoryAt = tl.lastHistAt
			for reason, n := range tl.reasons {
				report.DeclineReasons[reason] += n
				line.DeclineReasons = append(line.DeclineReasons, reason)
			}
			sort.Strings(line.DeclineReasons)
			events = append(events, tl.events...)

			if tl.lastAttemptAt != nil && (report.LastProviderAttempt == nil || tl.lastAttemptAt.After(*report.LastProviderAttempt)) {
				report.LastProviderAttempt = tl.lastAttemptAt
			}
			if tl.lastHistAttemptAt != nil && (report.LastHistoryAttempt == nil || tl.lastHistAttemptAt.After(*report.LastHistoryAttempt)) {
				report.LastHistoryAttempt = tl.lastHistAttemptAt
			}
		}
		if s.LastRetryAt != nil {
			events = append(events, TimelineEvent{
				At: s.LastRetryAt.UTC(), Source: "local", Kind: "dunning_retry",
				Detail: fmt.Sprintf("retry_attempts=%d", s.RetryAttempts),
			})
		}
		sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
		if len(events) > timelineCap {
			events = events[len(events)-timelineCap:] // keep the most recent
		}
		line.Timeline = events

		// History evidence of charge attempts counts as remote declines for
		// classification when the provider window saw nothing (the migrated
		// legacy events ARE the decline record for years the provider API
		// cannot serve).
		remoteDeclineEvidence := line.RemoteDeclines > 0 || line.HistoryFailures > 0
		attempted := s.RetryAttempts > 0 || s.LastRetryAt != nil
		switch {
		case !attempted && remoteDeclineEvidence:
			line.Classification = "never_attempted"
			report.NeverAttempted++
		case attempted && (s.NextRetryAt == nil || s.Status == "cancelled"):
			line.Classification = "attempted_and_exhausted"
			report.AttemptedExhausted++
		case attempted:
			line.Classification = "attempted_in_progress"
			report.AttemptedInProgress++
		default:
			line.Classification = "no_remote_declines"
			report.NoRemoteDeclines++
		}

		if s.LastRetryAt != nil && (report.LastLocalDunningAction == nil || s.LastRetryAt.After(*report.LastLocalDunningAction)) {
			report.LastLocalDunningAction = s.LastRetryAt
		}

		if len(report.Details) < forensicsDetailCap {
			report.Details = append(report.Details, line)
		}
	}
	report.DetailsCovered = len(report.Details)
	if len(report.DeclineReasons) == 0 {
		report.DeclineReasons = nil
	}

	// "Last dunning action per ANY source."
	type sourced struct {
		at  *time.Time
		via string
	}
	for _, c := range []sourced{
		{report.LastProviderAttempt, "provider"},
		{report.LastLocalDunningAction, "local"},
		{report.LastHistoryAttempt, "history"},
	} {
		if c.at == nil {
			continue
		}
		if report.LastDunningActionAnySource == nil || c.at.After(*report.LastDunningActionAnySource) {
			report.LastDunningActionAnySource = c.at
			report.LastDunningActionVia = c.via
		}
	}
	return report
}
