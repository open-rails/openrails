// Package progress carries the observed-progress handle for a unit of
// long-running work (xs-007). It is a leaf package so any layer — a River
// worker, the intent runner, the reconcile engine — can report progress on
// the context it was given without knowing who is watching.
//
// The watcher (riverjobs.JobLivenessMiddleware) decides whether work is
// wedged from these marks, never from elapsed time: "a clock reading is not
// a death certificate" (jobs_dunning.go); silence is.
package progress

import (
	"context"
	"sync/atomic"
	"time"
)

// Tracker records observed progress for one unit of work.
type Tracker struct {
	marks    atomic.Int64
	lastMark atomic.Int64 // unix nanos of the newest Mark, or the start
	note     atomic.Pointer[string]
	noop     bool
}

// NewTracker starts a tracker whose first observation is the start itself.
func NewTracker(start time.Time) *Tracker {
	t := &Tracker{}
	t.lastMark.Store(start.UnixNano())
	return t
}

// Mark records one observed unit of progress. note is free text for the
// forensic record ("merchant 37/500", "window 3/8"); it is what a reaper
// quotes when the work is stopped for silence.
func (t *Tracker) Mark(note string) {
	if t == nil || t.noop {
		return
	}
	t.marks.Add(1)
	t.lastMark.Store(time.Now().UnixNano())
	if note != "" {
		n := note
		t.note.Store(&n)
	}
}

// Marks is the number of progress reports so far.
func (t *Tracker) Marks() int64 {
	if t == nil {
		return 0
	}
	return t.marks.Load()
}

// LastMark is when progress was last observed (the start until the first
// Mark).
func (t *Tracker) LastMark() time.Time {
	if t == nil {
		return time.Time{}
	}
	return time.Unix(0, t.lastMark.Load()).UTC()
}

// LastNote is the newest note, "" before any.
func (t *Tracker) LastNote() string {
	if t == nil {
		return ""
	}
	if n := t.note.Load(); n != nil {
		return *n
	}
	return ""
}

type ctxKey struct{}

// WithTracker attaches t to ctx.
func WithTracker(ctx context.Context, t *Tracker) context.Context {
	return context.WithValue(ctx, ctxKey{}, t)
}

// From returns the tracker on ctx. A context nobody is watching (unit tests,
// direct calls) gets a no-op tracker, so callers never nil-check.
func From(ctx context.Context) *Tracker {
	if t, ok := ctx.Value(ctxKey{}).(*Tracker); ok && t != nil {
		return t
	}
	return &Tracker{noop: true}
}

// Mark is the one-liner callers use after each unit of work.
func Mark(ctx context.Context, note string) {
	From(ctx).Mark(note)
}
