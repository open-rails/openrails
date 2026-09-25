// Package lifecycle is the subscription state machine: one pure function from
// (snapshot, event) to (next snapshot, effects). Nothing here reads a clock,
// a database or a provider. Callers load the row under its lock, apply one
// event, persist the next snapshot and carry out the effects in the same
// transaction (#1089 design §3).
//
// Rules the table encodes:
//   - only a payment fact extends the paid period or restores an unpaid one;
//   - only a decline fact opens dunning;
//   - a clock reading alone moves a row to unverified, never further;
//   - access is kept through dunning and verification (the default policy);
//     only a confirmed outcome ends it.
package lifecycle

import (
	"errors"
	"fmt"
	"time"
)

// Status is a subscription's lifecycle state.
type Status string

const (
	Pending        Status = "pending"
	Active         Status = "active"
	PastDue        Status = "past_due"
	AwaitingMethod Status = "awaiting_method"
	Unverified     Status = "unverified"
	Cancelled      Status = "cancelled"
)

// Live reports whether the subscription still bills or may bill.
func (s Status) Live() bool {
	return s == Active || s == PastDue || s == AwaitingMethod || s == Unverified
}

// Owner is who charges renewals and who retries declines (design §2).
type Owner string

const (
	// Engine: OpenRails charges and duns.
	Engine Owner = "engine"
	// NMISchedule: NMI's schedule charges; OpenRails duns every decline.
	NMISchedule Owner = "nmi_schedule"
	// Provider: Stripe Billing or CCBill charge and retry; OpenRails mirrors.
	Provider Owner = "provider"
)

// Snapshot is the part of a subscription the machine decides on.
type Snapshot struct {
	Status Status
	Owner  Owner
	// PaidThrough is the end of the last paid period (current_period_ends_at).
	PaidThrough time.Time
	// EndedAt is when access ends, set on cancellation. A period-end
	// cancellation ends at PaidThrough and stays resumable until then.
	EndedAt time.Time
	// CancelKind records why a cancelled subscription ended.
	CancelKind CancelKind
}

// Bucket is the decline doctrine's answer (or#870, collection.DeclineOutcome).
type Bucket int

const (
	// Retry: ours or transient; keep the dunning schedule.
	Retry Bucket = iota
	// FixMethod: the customer's card needs fixing; stop charging until a new
	// one arrives, within the dunning window.
	FixMethod
	// NonRecoverable: the mandate is gone; cancel at the rail.
	NonRecoverable
)

// CancelKind is the recorded reason a subscription was cancelled.
type CancelKind string

const (
	CancelUser       CancelKind = "user"
	CancelMerchant   CancelKind = "merchant"
	CancelChargeback CancelKind = "chargeback"
	CancelExpired    CancelKind = "expired"
	CancelProvider   CancelKind = "provider"
	CancelAbandoned  CancelKind = "abandoned"
)

// Event is one fact or command applied to a subscription.
type Event interface{ event() string }

// InitialPaid is the first payment: the subscription starts.
type InitialPaid struct{ PeriodStart, PeriodEnd time.Time }

// InitialFailed is a first payment that did not complete, decided at At.
type InitialFailed struct{ At time.Time }

// RenewalPaid is a payment fact for the period [PeriodStart, PeriodEnd).
type RenewalPaid struct{ PeriodStart, PeriodEnd time.Time }

// RenewalDeclined is a decline fact, observed at At, for the period starting
// at PeriodStart.
type RenewalDeclined struct {
	PeriodStart time.Time
	Bucket      Bucket
	At          time.Time
}

// Reinstate reactivates a cancelled subscription with a paid period on an
// explicit decision (an operator override, a won dispute).
type Reinstate struct{ PeriodStart, PeriodEnd time.Time }

// MethodReplaced is the customer replacing the card of an awaiting subscription.
type MethodReplaced struct{}

// DunningExhausted is the dunning schedule spent after real declines.
type DunningExhausted struct{ At time.Time }

// TerminalHeld is a terminal outcome (a non-recoverable decline or a spent
// schedule) the operator's kill switch or a missing certainty leg refused:
// nothing more is charged, nothing is cancelled, access is kept.
type TerminalHeld struct{}

// ProviderConfirmedCurrent is the provider confirming an unverified
// subscription is alive while its paid period is still running. It restores
// the state and extends nothing.
type ProviderConfirmedCurrent struct{ At time.Time }

// DunningStale is a dunning case whose window closed with no payment and no
// spent schedule (the charges could not run). Nothing is known, so the row is
// verified at the provider; access is kept.
type DunningStale struct{}

// RenewalOverdue is the clock passing PaidThrough with no fact for the next
// period. It is not evidence; it only asks the provider.
type RenewalOverdue struct{}

// ProviderCancelled is the provider confirming the subscription ended.
type ProviderCancelled struct{ At time.Time }

// Cancel is a user, merchant or chargeback cancellation.
type Cancel struct {
	Kind CancelKind
	// Immediate ends access at At; otherwise access runs to PaidThrough.
	Immediate bool
	At        time.Time
}

// Resume reverses a user's period-end cancellation inside the paid period.
type Resume struct{ At time.Time }

func (InitialPaid) event() string      { return "initial_paid" }
func (InitialFailed) event() string    { return "initial_failed" }
func (RenewalPaid) event() string      { return "renewal_paid" }
func (RenewalDeclined) event() string  { return "renewal_declined" }
func (MethodReplaced) event() string   { return "method_replaced" }
func (Reinstate) event() string        { return "reinstate" }
func (DunningExhausted) event() string { return "dunning_exhausted" }
func (TerminalHeld) event() string     { return "terminal_held" }
func (DunningStale) event() string     { return "dunning_stale" }
func (ProviderConfirmedCurrent) event() string {
	return "provider_confirmed_current"
}
func (RenewalOverdue) event() string    { return "renewal_overdue" }
func (ProviderCancelled) event() string { return "provider_cancelled" }
func (Cancel) event() string            { return "cancel" }
func (Resume) event() string            { return "resume" }

// Name is the event's stable name for the transition log.
func Name(e Event) string { return e.event() }

// Effect is work the caller performs in the transition's transaction.
type Effect interface{ effect() string }

// GrantPeriod records paid access for the period.
type GrantPeriod struct{ Start, End time.Time }

// EndAccess closes access at At.
type EndAccess struct{ At time.Time }

// OpenDunning starts the dunning case for the unpaid period.
type OpenDunning struct{ PeriodStart time.Time }

// CloseDunning ends any dunning case (paid, cancelled, awaiting the customer).
type CloseDunning struct{}

// QueueProviderCancel asks the provider to stop its schedule (durable intent).
type QueueProviderCancel struct{}

// ReopenAccess reopens access a period-end cancellation had bounded.
type ReopenAccess struct{}

// ProbeProvider reads the provider now to resolve an unverified row (§12).
type ProbeProvider struct{}

// Notify tells the customer.
type Notify struct{ Kind NoticeKind }

// NoticeKind names a customer notification.
type NoticeKind string

const (
	NoticeStarted       NoticeKind = "premium_started"
	NoticeRenewed       NoticeKind = "premium_renewed"
	NoticePaymentFailed NoticeKind = "payment_method_failed"
	NoticeUpdateMethod  NoticeKind = "payment_method_update_required"
	NoticeEnded         NoticeKind = "premium_ended"
)

func (GrantPeriod) effect() string         { return "grant_period" }
func (EndAccess) effect() string           { return "end_access" }
func (OpenDunning) effect() string         { return "open_dunning" }
func (CloseDunning) effect() string        { return "close_dunning" }
func (QueueProviderCancel) effect() string { return "queue_provider_cancel" }
func (ReopenAccess) effect() string        { return "reopen_access" }
func (ProbeProvider) effect() string       { return "probe_provider" }
func (Notify) effect() string              { return "notify" }

var (
	// ErrIllegal is an event the state does not accept.
	ErrIllegal = errors.New("lifecycle: illegal transition")
	// ErrTerminal is a payment for a cancelled subscription: record the money,
	// never reactivate; the charge goes to refund review.
	ErrTerminal = errors.New("lifecycle: payment for a cancelled subscription")
	// ErrInvalid is an event with contradictory fields.
	ErrInvalid = errors.New("lifecycle: invalid event")
)

// Apply decides one event. A fact about a period that is already settled
// (stale or replayed) returns the snapshot unchanged with no effects.
func Apply(s Snapshot, e Event) (Snapshot, []Effect, error) {
	switch ev := e.(type) {
	case InitialPaid:
		if !ev.PeriodEnd.After(ev.PeriodStart) {
			return s, nil, invalid(e, "empty period")
		}
		if s.Status != Pending {
			if s.Status.Live() && !ev.PeriodEnd.After(s.PaidThrough) {
				return s, nil, nil // replayed
			}
			return s, nil, illegal(s, e)
		}
		s.Status, s.PaidThrough = Active, ev.PeriodEnd
		return s, []Effect{GrantPeriod{ev.PeriodStart, ev.PeriodEnd}, Notify{NoticeStarted}}, nil

	case InitialFailed:
		if ev.At.IsZero() {
			return s, nil, invalid(e, "decision instant required")
		}
		if s.Status != Pending {
			return s, nil, illegal(s, e)
		}
		s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelAbandoned, ev.At
		return s, nil, nil

	case RenewalPaid:
		if !ev.PeriodEnd.After(ev.PeriodStart) {
			return s, nil, invalid(e, "empty period")
		}
		if !ev.PeriodEnd.After(s.PaidThrough) {
			return s, nil, nil // already paid through this period
		}
		if s.Status == Cancelled {
			if s.CancelKind != CancelExpired {
				return s, nil, ErrTerminal
			}
			// A lapsed (not a decided) end that the provider billed anyway:
			// the payment restores the subscription.
			s.Status, s.CancelKind, s.EndedAt, s.PaidThrough = Active, "", time.Time{}, ev.PeriodEnd
			return s, []Effect{GrantPeriod{ev.PeriodStart, ev.PeriodEnd}, ReopenAccess{}, Notify{NoticeRenewed}}, nil
		}
		if !s.Status.Live() {
			return s, nil, illegal(s, e)
		}
		wasDunning := s.Status != Active
		s.Status, s.PaidThrough = Active, ev.PeriodEnd
		effects := []Effect{GrantPeriod{ev.PeriodStart, ev.PeriodEnd}}
		if wasDunning {
			effects = append(effects, CloseDunning{})
		}
		return s, append(effects, Notify{NoticeRenewed}), nil

	case RenewalDeclined:
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, nil // a late decline never touches a cancelled row
		}
		if !ev.PeriodStart.Equal(s.PaidThrough) {
			return s, nil, nil // a decline for a period already paid or not yet due
		}
		if s.Owner == Provider {
			s.Status = PastDue // the provider retries; mirror only
			return s, nil, nil
		}
		switch ev.Bucket {
		case FixMethod:
			// Charging stops until the member adds a card; the dunning window
			// stays open, so the wait ends at exhaustion like any decline.
			if s.Status == AwaitingMethod {
				return s, nil, nil
			}
			effects := []Effect{Notify{NoticeUpdateMethod}}
			if s.Status != PastDue {
				effects = append([]Effect{OpenDunning{ev.PeriodStart}}, effects...)
			}
			s.Status = AwaitingMethod
			return s, effects, nil
		case NonRecoverable:
			end := s.PaidThrough
			if ev.At.After(end) {
				end = ev.At
			}
			s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelExpired, end
			return s, []Effect{CloseDunning{}, EndAccess{end}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil
		default:
			effects := []Effect{Notify{NoticePaymentFailed}}
			if s.Status != PastDue {
				effects = append([]Effect{OpenDunning{ev.PeriodStart}}, effects...)
			}
			s.Status = PastDue
			return s, effects, nil
		}

	case Reinstate:
		if (s.Status != Cancelled && !s.Status.Live()) || !ev.PeriodEnd.After(ev.PeriodStart) {
			return s, nil, illegal(s, e)
		}
		s.Status, s.CancelKind, s.EndedAt = Active, "", time.Time{}
		if ev.PeriodEnd.After(s.PaidThrough) {
			s.PaidThrough = ev.PeriodEnd
		}
		return s, []Effect{GrantPeriod{ev.PeriodStart, ev.PeriodEnd}, ReopenAccess{}}, nil

	case MethodReplaced:
		if s.Status != AwaitingMethod {
			return s, nil, nil
		}
		s.Status = PastDue
		return s, []Effect{OpenDunning{s.PaidThrough}}, nil

	case DunningExhausted:
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, illegal(s, e)
		}
		s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelExpired, ev.At
		return s, []Effect{CloseDunning{}, EndAccess{ev.At}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil

	case TerminalHeld:
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, nil
		}
		if s.Owner == Engine {
			s.Status = PastDue // the engine's own obligation waits for the operator
			return s, []Effect{CloseDunning{}}, nil
		}
		s.Status = Unverified
		return s, []Effect{CloseDunning{}, ProbeProvider{}}, nil

	case ProviderConfirmedCurrent:
		if s.Status != Unverified || !s.PaidThrough.After(ev.At) {
			return s, nil, nil
		}
		s.Status = Active
		return s, nil, nil

	case DunningStale:
		if s.Status != PastDue && s.Status != Active {
			return s, nil, nil
		}
		if s.Owner == Engine {
			return s, nil, nil // the engine's own obligation stays with its due pass
		}
		s.Status = Unverified
		return s, []Effect{CloseDunning{}, ProbeProvider{}}, nil

	case RenewalOverdue:
		if s.Status != Active || s.Owner == Engine {
			return s, nil, nil // the engine's due pass owns its own renewals
		}
		s.Status = Unverified
		return s, []Effect{ProbeProvider{}}, nil

	case ProviderCancelled:
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, nil
		}
		end := ev.At
		if s.PaidThrough.After(end) {
			end = s.PaidThrough // what was paid for is kept
		}
		s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelProvider, end
		return s, []Effect{CloseDunning{}, EndAccess{end}, Notify{NoticeEnded}}, nil

	case Cancel:
		if ev.Kind == "" {
			return s, nil, invalid(e, "cancel kind required")
		}
		if s.Status == Cancelled {
			// A later immediate cancel (a chargeback, a merchant revoke) ends
			// paid access now; otherwise the first decided cancel stands.
			if (ev.Immediate || ev.Kind == CancelChargeback) && s.EndedAt.After(ev.At) {
				s.CancelKind, s.EndedAt = ev.Kind, ev.At
				return s, []Effect{EndAccess{ev.At}}, nil
			}
			return s, nil, nil
		}
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, illegal(s, e)
		}
		end := ev.At
		if !ev.Immediate && ev.Kind != CancelChargeback && s.PaidThrough.After(ev.At) {
			end = s.PaidThrough // access runs to the end of what was paid
		}
		s.Status, s.CancelKind, s.EndedAt = Cancelled, ev.Kind, end
		return s, []Effect{CloseDunning{}, EndAccess{end}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil

	case Resume:
		resumable := s.CancelKind == CancelUser || s.CancelKind == CancelMerchant || s.CancelKind == CancelExpired
		if s.Status == Cancelled && resumable && !s.EndedAt.Before(s.PaidThrough) && s.PaidThrough.After(ev.At) {
			s.Status, s.CancelKind, s.EndedAt = Active, "", time.Time{}
			return s, []Effect{ReopenAccess{}}, nil
		}
		return s, nil, illegal(s, e)
	}
	return s, nil, fmt.Errorf("%w: unknown event %T", ErrInvalid, e)
}

// RenewalDue reports whether the engine's due pass should open the renewal
// obligation for this subscription at now.
func RenewalDue(s Snapshot, now time.Time) bool {
	return s.Owner == Engine && s.Status == Active && !now.Before(s.PaidThrough)
}

func illegal(s Snapshot, e Event) error {
	return fmt.Errorf("%w: %s from %s", ErrIllegal, e.event(), s.Status)
}

func invalid(e Event, why string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, e.event(), why)
}
