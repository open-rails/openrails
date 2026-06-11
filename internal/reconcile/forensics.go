package reconcile

import (
	"sort"
	"time"
)

// DunningForensics answers, per provider: did dunning ever run, when did it
// stop, and did attempts fail? For every locally past_due or
// cancelled-as-expired subscription it lines up the processor's
// charge-attempt timeline (declines included) against the local retry fields
// (last_retry_at / retry_attempts / next_retry_at).
type DunningForensics struct {
	Provider              Provider   `json:"provider"`
	SubscriptionsExamined int        `json:"subscriptions_examined"`
	// NeverAttempted: the processor recorded declines but local dunning never
	// acted (retry fields frozen/null) — "dunning never ran".
	NeverAttempted int `json:"never_attempted"`
	// AttemptedExhausted: local dunning acted and gave up (attempts > 0, no
	// next retry scheduled or the subscription is already terminal).
	AttemptedExhausted int `json:"attempted_and_exhausted"`
	// AttemptedInProgress: local dunning acted and still has a retry queued.
	AttemptedInProgress int `json:"attempted_in_progress"`
	// NoRemoteDeclines: neither the processor nor local dunning shows
	// attempts in the examined window.
	NoRemoteDeclines int `json:"no_remote_declines"`
	// LastLocalDunningAction is the newest last_retry_at across the examined
	// set — when local dunning last did anything at all.
	LastLocalDunningAction *time.Time     `json:"last_local_dunning_action,omitempty"`
	DeclineReasons         map[string]int `json:"decline_reason_histogram,omitempty"`
	// Details are per-subscription lines, capped at forensicsDetailCap.
	Details        []DunningSubscriptionReport `json:"details,omitempty"`
	DetailsCovered int                         `json:"details_covered"`
}

// DunningSubscriptionReport is one subscription's cross-referenced timeline.
type DunningSubscriptionReport struct {
	SubscriptionID          string     `json:"subscription_id"`
	ProcessorSubscriptionID string     `json:"processor_subscription_id,omitempty"`
	Status                  string     `json:"status"`
	CancelType              string     `json:"cancel_type,omitempty"`
	RetryAttempts           int        `json:"retry_attempts"`
	LastRetryAt             *time.Time `json:"last_retry_at,omitempty"`
	NextRetryAt             *time.Time `json:"next_retry_at,omitempty"`
	RemoteDeclines          int        `json:"remote_declines"`
	RemoteSuccesses         int        `json:"remote_successes"`
	FirstDeclineAt          *time.Time `json:"first_decline_at,omitempty"`
	LastDeclineAt           *time.Time `json:"last_decline_at,omitempty"`
	DeclineReasons          []string   `json:"decline_reasons,omitempty"`
	Classification          string     `json:"classification"`
}

const forensicsDetailCap = 200

// computeDunningForensics builds the per-provider dunning report from the
// snapshot's transaction timeline and the local retry fields.
func computeDunningForensics(provider Provider, snap *RemoteSnapshot, local *LocalState, now time.Time) *DunningForensics {
	idx := buildLocalIndex(local)
	ridx := buildRemoteIndex(snap)
	corr := &correlator{local: idx, remote: ridx}

	// Correlate the remote charge timeline to local subscriptions once.
	type timeline struct {
		declines, successes            int
		firstDeclineAt, lastDeclineAt  *time.Time
		reasons                        map[string]int
	}
	timelines := map[string]*timeline{} // keyed by local subscription id
	for i := range snap.Transactions {
		t := &snap.Transactions[i]
		if t.Type != TransactionTypeSale && t.Type != TransactionTypeDecline {
			continue
		}
		sub, _, _ := corr.subForTxn(t)
		if sub == nil {
			continue
		}
		tl := timelines[sub.ID.String()]
		if tl == nil {
			tl = &timeline{reasons: map[string]int{}}
			timelines[sub.ID.String()] = tl
		}
		if t.Success {
			tl.successes++
			continue
		}
		tl.declines++
		if !t.OccurredAt.IsZero() {
			at := t.OccurredAt.UTC()
			if tl.firstDeclineAt == nil || at.Before(*tl.firstDeclineAt) {
				tl.firstDeclineAt = &at
			}
			if tl.lastDeclineAt == nil || at.After(*tl.lastDeclineAt) {
				tl.lastDeclineAt = &at
			}
		}
		reason := t.DeclineReason
		if reason == "" {
			reason = "(no decline reason)"
		}
		tl.reasons[reason]++
	}

	report := &DunningForensics{
		Provider:       provider,
		DeclineReasons: map[string]int{},
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
			SubscriptionID:          s.ID.String(),
			ProcessorSubscriptionID: s.ProcessorSubscriptionID,
			Status:                  s.Status,
			CancelType:              s.CancelType,
			RetryAttempts:           s.RetryAttempts,
			LastRetryAt:             s.LastRetryAt,
			NextRetryAt:             s.NextRetryAt,
		}
		if tl != nil {
			line.RemoteDeclines = tl.declines
			line.RemoteSuccesses = tl.successes
			line.FirstDeclineAt = tl.firstDeclineAt
			line.LastDeclineAt = tl.lastDeclineAt
			for reason, n := range tl.reasons {
				report.DeclineReasons[reason] += n
				line.DeclineReasons = append(line.DeclineReasons, reason)
			}
			sort.Strings(line.DeclineReasons)
		}

		attempted := s.RetryAttempts > 0 || s.LastRetryAt != nil
		switch {
		case !attempted && line.RemoteDeclines > 0:
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
	return report
}
