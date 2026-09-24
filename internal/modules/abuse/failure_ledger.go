package abuse

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// FailureLedger is the durable card-testing counter (SEC-30). Every replica
// reads and writes the same PostgreSQL rows, so blocks hold across a fleet
// without Redis; CardAbuseGuard stays an optional captcha accelerator.
//
// Per subject (customer, client address): BlockAfter failures in FailWindow
// or DailyBlockAfter in DailyWindow block further card attempts. Merchant-wide
// GlobalAttackAfter failures in GlobalWindow is attack mode: any subject with
// a failure inside FailWindow is blocked.
type FailureLedger struct {
	db    *db.DB
	clock clockwork.Clock
	cfg   CardAbuseConfig
}

const failureBucket = 5 * time.Minute

// MerchantSubject is the merchant-wide row that detects attack mode.
const MerchantSubject = "merchant"

// NewFailureLedger requires the runtime's database and clock.
func NewFailureLedger(database *db.DB, clock clockwork.Clock, cfg CardAbuseConfig) *FailureLedger {
	if database == nil || clock == nil {
		return nil
	}
	return &FailureLedger{db: database, clock: clock, cfg: cfg}
}

// CustomerSubject and AddressSubject name the ledger's per-subject rows.
func CustomerSubject(id string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(id)); err == nil {
		id = parsed.String()
	}
	return subject("customer:", id)
}
func AddressSubject(ip string) string { return subject("ip:", ip) }

func subject(prefix, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return prefix + v
}

func cleanSubjects(subjects []string) []string {
	out := make([]string, 0, len(subjects)+1)
	for _, s := range subjects {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Record counts one failed card attempt for each subject. Callers name
// MerchantSubject exactly once per attempt.
func (l *FailureLedger) Record(ctx context.Context, merchantID uuid.UUID, subjects ...string) error {
	subjects = cleanSubjects(subjects)
	if l == nil || l.db == nil || merchantID == uuid.Nil || len(subjects) == 0 {
		return nil
	}
	now := l.clock.Now().UTC()
	q := l.db.Gen(ctx)
	if err := q.RecordCardAttemptFailure(ctx, gen.RecordCardAttemptFailureParams{
		MerchantID: merchantID, BucketAt: now.Truncate(failureBucket), Subjects: subjects,
	}); err != nil {
		return err
	}
	_, err := q.PruneCardAttemptFailures(ctx, gen.PruneCardAttemptFailuresParams{MerchantID: merchantID, Before: now.Add(-l.horizon())})
	return err
}

func (l *FailureLedger) horizon() time.Duration {
	return max(l.cfg.DailyWindow, l.cfg.GlobalWindow, l.cfg.FailWindow) + failureBucket
}

// Blocked reports whether any subject may not attempt another card now, and
// for how long it should wait.
func (l *FailureLedger) Blocked(ctx context.Context, merchantID uuid.UUID, subjects ...string) (time.Duration, bool, error) {
	if l == nil || l.db == nil || merchantID == uuid.Nil {
		return 0, false, nil
	}
	subjects = cleanSubjects(append(subjects, MerchantSubject))
	now := l.clock.Now().UTC()
	rows, err := l.db.Gen(ctx).CardAttemptFailureCounts(ctx, gen.CardAttemptFailureCountsParams{
		BurstSince: now.Add(-l.cfg.FailWindow),
		MerchantID: merchantID,
		Subjects:   subjects,
		DailySince: now.Add(-max(l.cfg.DailyWindow, l.cfg.GlobalWindow)),
	})
	if err != nil {
		return 0, false, err
	}
	attack := false
	for _, row := range rows {
		if row.Subject == MerchantSubject && row.Daily >= l.cfg.GlobalAttackAfter {
			attack = true
		}
	}
	var wait time.Duration
	for _, row := range rows {
		if row.Subject == MerchantSubject {
			continue
		}
		if row.Daily >= l.cfg.DailyBlockAfter {
			wait = max(wait, l.cfg.DailyWindow)
		}
		if row.Burst >= l.cfg.BlockAfter || (attack && row.Burst > 0) {
			wait = max(wait, l.cfg.FailWindow)
		}
	}
	return wait, wait > 0, nil
}
