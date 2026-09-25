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
	// CancelAtPeriodEnd is a scheduled, still reversible cancellation.
	CancelAtPeriodEnd bool
	// EndedAt is when access ended, set on cancellation.
	EndedAt time.Time
	// CancelKind records why a cancelled subscription ended.
	CancelKind CancelKind
}

// Bucket is the decline doctrine's answer (or#870, collection.DeclineOutcome).
type Bucket int

const (
	// Retry: ours or transient; keep the dunning schedule.
	Retry Bucket = iota
	// FixMethod: the customer's card needs fixing; stop charging, keep access.
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

// InitialFailed is a first payment that did not complete.
type InitialFailed struct{}

// RenewalPaid is a payment fact for the period [PeriodStart, PeriodEnd).
type RenewalPaid struct{ PeriodStart, PeriodEnd time.Time }

// RenewalDeclined is a decline fact for the period starting at PeriodStart.
type RenewalDeclined struct {
	PeriodStart time.Time
	Bucket      Bucket
}

// MethodReplaced is the customer replacing the card of an awaiting subscription.
type MethodReplaced struct{}

// DunningExhausted is the dunning schedule spent after real declines.
type DunningExhausted struct{ At time.Time }

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

// Resume reverses a period-end cancellation inside the paid period.
type Resume struct{ At time.Time }

// PeriodEnded is the clock reaching PaidThrough. It only completes a
// period-end cancellation; renewal is decided by facts.
type PeriodEnded struct{ At time.Time }

func (InitialPaid) event() string       { return "initial_paid" }
func (InitialFailed) event() string     { return "initial_failed" }
func (RenewalPaid) event() string       { return "renewal_paid" }
func (RenewalDeclined) event() string   { return "renewal_declined" }
func (MethodReplaced) event() string    { return "method_replaced" }
func (DunningExhausted) event() string  { return "dunning_exhausted" }
func (RenewalOverdue) event() string    { return "renewal_overdue" }
func (ProviderCancelled) event() string { return "provider_cancelled" }
func (Cancel) event() string            { return "cancel" }
func (Resume) event() string            { return "resume" }
func (PeriodEnded) event() string       { return "period_ended" }

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
		if s.Status != Pending {
			return s, nil, illegal(s, e)
		}
		s.Status, s.CancelKind = Cancelled, CancelAbandoned
		return s, nil, nil

	case RenewalPaid:
		if !ev.PeriodEnd.After(ev.PeriodStart) {
			return s, nil, invalid(e, "empty period")
		}
		if !ev.PeriodEnd.After(s.PaidThrough) {
			return s, nil, nil // already paid through this period
		}
		if s.Status == Cancelled {
			return s, nil, ErrTerminal
		}
		if !s.Status.Live() {
			return s, nil, illegal(s, e)
		}
		if ev.PeriodStart.Before(s.PaidThrough) {
			return s, nil, invalid(e, "period overlaps the paid period")
		}
		wasDunning := s.Status != Active
		s.Status, s.PaidThrough = Active, ev.PeriodEnd
		effects := []Effect{GrantPeriod{ev.PeriodStart, ev.PeriodEnd}}
		if wasDunning {
			effects = append(effects, CloseDunning{})
		}
		return s, append(effects, Notify{NoticeRenewed}), nil

	case RenewalDeclined:
		if !s.Status.Live() {
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
			if s.Status == AwaitingMethod {
				return s, nil, nil
			}
			s.Status = AwaitingMethod
			return s, []Effect{CloseDunning{}, Notify{NoticeUpdateMethod}}, nil
		case NonRecoverable:
			s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelExpired, s.PaidThrough
			return s, []Effect{CloseDunning{}, EndAccess{s.PaidThrough}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil
		default:
			effects := []Effect{Notify{NoticePaymentFailed}}
			if s.Status != PastDue {
				effects = append([]Effect{OpenDunning{ev.PeriodStart}}, effects...)
			}
			s.Status = PastDue
			return s, effects, nil
		}

	case MethodReplaced:
		if s.Status != AwaitingMethod {
			return s, nil, nil
		}
		s.Status = PastDue
		return s, []Effect{OpenDunning{s.PaidThrough}}, nil

	case DunningExhausted:
		if s.Status != PastDue {
			return s, nil, illegal(s, e)
		}
		s.Status, s.CancelKind, s.EndedAt = Cancelled, CancelExpired, ev.At
		return s, []Effect{CloseDunning{}, EndAccess{ev.At}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil

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
			if ev.Kind == CancelChargeback && s.CancelKind != CancelChargeback && s.EndedAt.After(ev.At) {
				s.CancelKind, s.EndedAt = CancelChargeback, ev.At // a chargeback ends paid access now
				return s, []Effect{EndAccess{ev.At}}, nil
			}
			return s, nil, nil
		}
		if !s.Status.Live() && s.Status != Pending {
			return s, nil, illegal(s, e)
		}
		immediate := ev.Immediate || ev.Kind == CancelChargeback || !s.PaidThrough.After(ev.At)
		if !immediate {
			if s.CancelAtPeriodEnd {
				return s, nil, nil
			}
			s.CancelAtPeriodEnd = true
			return s, []Effect{CloseDunning{}, QueueProviderCancel{}}, nil
		}
		s.Status, s.CancelKind, s.EndedAt, s.CancelAtPeriodEnd = Cancelled, ev.Kind, ev.At, false
		return s, []Effect{CloseDunning{}, EndAccess{ev.At}, QueueProviderCancel{}, Notify{NoticeEnded}}, nil

	case PeriodEnded:
		if !s.Status.Live() || !s.CancelAtPeriodEnd || ev.At.Before(s.PaidThrough) {
			return s, nil, nil
		}
		s.Status, s.CancelKind, s.EndedAt, s.CancelAtPeriodEnd = Cancelled, CancelUser, s.PaidThrough, false
		return s, []Effect{CloseDunning{}, EndAccess{s.PaidThrough}, Notify{NoticeEnded}}, nil

	case Resume:
		if s.Status.Live() && s.CancelAtPeriodEnd && s.PaidThrough.After(ev.At) {
			s.CancelAtPeriodEnd = false
			return s, nil, nil
		}
		return s, nil, illegal(s, e)
	}
	return s, nil, fmt.Errorf("%w: unknown event %T", ErrInvalid, e)
}

// RenewalDue reports whether the engine's due pass should open the renewal
// obligation for this subscription at now.
func RenewalDue(s Snapshot, now time.Time) bool {
	return s.Owner == Engine && s.Status == Active && !s.CancelAtPeriodEnd && !now.Before(s.PaidThrough)
}

func illegal(s Snapshot, e Event) error {
	return fmt.Errorf("%w: %s from %s", ErrIllegal, e.event(), s.Status)
}

func invalid(e Event, why string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, e.event(), why)
}
