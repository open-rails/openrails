package riverjobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

func i64p(v int64) *int64  { return &v }
func healthNow() time.Time { return time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC) }
func agoAt(d time.Duration) *time.Time {
	t := healthNow().Add(-d)
	return &t
}

func TestEvaluateWorkerHealth(t *testing.T) {
	now := healthNow()
	const threshold, mult = 3, 3
	minStale := 10 * time.Minute

	cases := []struct {
		name string
		row  gen.OpenrailsWorkerHealth
		want string
	}{
		{"fresh row, no runs yet", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-time.Minute), ExpectedPeriodSeconds: i64p(3600)}, ""},
		{"consecutive failures trip", gen.OpenrailsWorkerHealth{RegisteredAt: now, ConsecutiveFailures: 3}, "consecutive_failures"},
		{"two failures do not trip", gen.OpenrailsWorkerHealth{RegisteredAt: now, ConsecutiveFailures: 2}, ""},
		{"never succeeded past grace", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-4 * time.Hour), ExpectedPeriodSeconds: i64p(3600)}, "never_succeeded"},
		{"never succeeded within grace", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-2 * time.Hour), ExpectedPeriodSeconds: i64p(3600)}, ""},
		{"stale periodic", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-24 * time.Hour), ExpectedPeriodSeconds: i64p(3600), LastSuccessAt: agoAt(4 * time.Hour)}, "stale"},
		{"on-time periodic", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-24 * time.Hour), ExpectedPeriodSeconds: i64p(3600), LastSuccessAt: agoAt(30 * time.Minute)}, ""},
		{"on-demand never alerts on cadence", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-240 * time.Hour)}, ""},
		{"min-stale floor beats k*period", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-24 * time.Hour), ExpectedPeriodSeconds: i64p(60), LastSuccessAt: agoAt(5 * time.Minute)}, ""},
		{"past min-stale floor", gen.OpenrailsWorkerHealth{RegisteredAt: now.Add(-24 * time.Hour), ExpectedPeriodSeconds: i64p(60), LastSuccessAt: agoAt(11 * time.Minute)}, "stale"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, evaluateWorkerHealth(tc.row, now, threshold, mult, minStale))
		})
	}
}

func TestWorkerAlertDue(t *testing.T) {
	now := healthNow()
	every := 24 * time.Hour

	require.True(t, workerAlertDue(gen.OpenrailsWorkerHealth{}, now, every), "first trip alerts")
	require.False(t, workerAlertDue(gen.OpenrailsWorkerHealth{LastAlertedAt: agoAt(time.Hour)}, now, every), "recently alerted waits")
	require.True(t, workerAlertDue(gen.OpenrailsWorkerHealth{LastAlertedAt: agoAt(25 * time.Hour)}, now, every), "re-alert pacing elapsed")
	// A success AFTER the last alert = a fresh incident.
	require.True(t, workerAlertDue(gen.OpenrailsWorkerHealth{LastAlertedAt: agoAt(2 * time.Hour), LastSuccessAt: agoAt(time.Hour)}, now, every))
}

func TestWorkerRegistrations(t *testing.T) {
	regs := NewWorkerRegistrations()
	regs.NoteKind("a")
	regs.NotePeriod("a", time.Hour)
	regs.NotePeriod("a", 30*24*time.Hour) // longer schedule for same kind: shortest wins
	regs.NotePeriod("b", 15*time.Minute)  // period without prior NoteKind still registers
	regs.NoteKind("b")                    // and NoteKind never clobbers a known period
	regs.NoteKind("c")                    // on-demand

	snap := regs.Snapshot()
	require.Equal(t, map[string]time.Duration{"a": time.Hour, "b": 15 * time.Minute, "c": 0}, snap)

	var nilRegs *WorkerRegistrations
	require.Nil(t, nilRegs.Snapshot())
	nilRegs.NoteKind("x") // nil-safe no-ops
	nilRegs.NotePeriod("x", time.Hour)
}

func TestTruncateRunes(t *testing.T) {
	require.Equal(t, "abc", truncateRunes("abc", 5))
	require.Equal(t, "ab", truncateRunes("abc", 2))
	require.Equal(t, "héll", truncateRunes("héllo", 4), "truncates runes, not bytes")
	require.Equal(t, "", truncateRunes("abc", 0))
}
