package abuse

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/abusestate"
)

// FailureLedger counts refused cards in the abuse state: in Redis, shared by
// every instance, else in this process's memory, for one instance.
//
// Per subject (customer, client address): BlockAfter failures in FailWindow or
// DailyBlockAfter in DailyWindow block card attempts. In attack mode any subject
// with a failure inside FailWindow is blocked. The windows slide, so blocks and
// attack mode end once failures age out.
type FailureLedger struct {
	state *abusestate.Store
	clock clockwork.Clock
	cfg   CardAbuseConfig
}

// MerchantSubject is the merchant-wide subject that detects attack mode.
const MerchantSubject = "merchant"

// NewFailureLedger counts in state on the runtime's clock.
func NewFailureLedger(state *abusestate.Store, clock clockwork.Clock, cfg CardAbuseConfig) *FailureLedger {
	if state == nil || clock == nil {
		return nil
	}
	return &FailureLedger{state: state, clock: clock, cfg: cfg}
}

const (
	customerPrefix = "customer:"
	addressPrefix  = "ip:"
)

// CustomerSubject and AddressSubject name the ledger's per-subject rows. An
// IPv6 client is one /64, the block a single subscriber is given.
func CustomerSubject(id string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(id)); err == nil {
		id = parsed.String()
	}
	return subject(customerPrefix, id)
}
func AddressSubject(ip string) string {
	if addr, err := netip.ParseAddr(strings.TrimSpace(ip)); err == nil && addr.Is6() && !addr.Is4In6() {
		ip = netip.PrefixFrom(addr.WithZone(""), 64).Masked().String()
	}
	return subject(addressPrefix, ip)
}

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
func (l *FailureLedger) Record(ctx context.Context, merchantID uuid.UUID, subjects ...string) {
	subjects = cleanSubjects(subjects)
	if l == nil || merchantID == uuid.Nil || len(subjects) == 0 {
		return
	}
	failure := uuid.NewString()
	marks := make([]abusestate.Mark, 0, 2*len(subjects))
	for _, subject := range subjects {
		if subject == MerchantSubject {
			marks = append(marks, abusestate.Mark{Set: attackKey(merchantID, "failures"), Member: failure, TTL: l.cfg.GlobalWindow})
			continue
		}
		marks = append(marks,
			abusestate.Mark{Set: subjectKey(merchantID, subject, "burst"), Member: failure, TTL: l.cfg.FailWindow},
			abusestate.Mark{Set: subjectKey(merchantID, subject, "day"), Member: failure, TTL: l.cfg.DailyWindow})
		switch {
		case strings.HasPrefix(subject, customerPrefix):
			marks = append(marks, abusestate.Mark{Set: attackKey(merchantID, "customers"), Member: subject, TTL: l.cfg.GlobalWindow})
		case strings.HasPrefix(subject, addressPrefix):
			marks = append(marks, abusestate.Mark{Set: attackKey(merchantID, "addresses"), Member: subject, TTL: l.cfg.GlobalWindow})
		}
	}
	l.state.Mark(ctx, l.clock.Now(), marks...)
}

// Blocked reports whether any subject may not attempt another card now, and
// for how long it should wait.
func (l *FailureLedger) Blocked(ctx context.Context, merchantID uuid.UUID, subjects ...string) (time.Duration, bool) {
	subjects = cleanSubjects(subjects)
	if l == nil || merchantID == uuid.Nil || len(subjects) == 0 {
		return 0, false
	}
	sets := make([]string, 0, 2*len(subjects))
	for _, subject := range subjects {
		sets = append(sets, subjectKey(merchantID, subject, "burst"), subjectKey(merchantID, subject, "day"))
	}
	counts := l.state.Marked(ctx, l.clock.Now(), sets...)
	var wait time.Duration
	attack, checked := false, false
	for i := 0; i < len(counts); i += 2 {
		burst, day := counts[i], counts[i+1]
		if day >= l.cfg.DailyBlockAfter {
			wait = max(wait, l.cfg.DailyWindow)
		}
		if burst > 0 && burst < l.cfg.BlockAfter && !checked {
			attack, checked = l.AttackMode(ctx, merchantID), true
		}
		if burst >= l.cfg.BlockAfter || (attack && burst > 0) {
			wait = max(wait, l.cfg.FailWindow)
		}
	}
	return wait, wait > 0
}

// AttackMode reports whether the merchant is under a card-testing attack:
// GlobalAttackAfter failures in GlobalWindow from at least
// GlobalAttackSubjects customers and as many client addresses.
func (l *FailureLedger) AttackMode(ctx context.Context, merchantID uuid.UUID) bool {
	if l == nil || merchantID == uuid.Nil {
		return false
	}
	n := l.state.Marked(ctx, l.clock.Now(), attackKey(merchantID, "failures"), attackKey(merchantID, "customers"), attackKey(merchantID, "addresses"))
	return n[0] >= l.cfg.GlobalAttackAfter && n[1] >= l.cfg.GlobalAttackSubjects && n[2] >= l.cfg.GlobalAttackSubjects
}

func subjectKey(merchantID uuid.UUID, subject, window string) string {
	return "card_declines:{" + merchantID.String() + "}:" + subject + ":" + window
}

func attackKey(merchantID uuid.UUID, what string) string {
	return "card_attack:{" + merchantID.String() + "}:" + what
}
