package abuse

import (
	"context"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/captcha"
)

// CardAbuseConfig tunes the failure-driven captcha/block escalation. Each
// subject has a burst window (FailWindow: CaptchaAfter -> captcha, BlockAfter
// -> block) and a daily window (DailyWindow: DailyBlockAfter -> blocked for the
// day). The Global* fields set merchant-wide attack mode, which the
// FailureLedger decides.
type CardAbuseConfig struct {
	// FailWindow is the short rolling window for per-subject failed-charge counting.
	FailWindow time.Duration
	// CaptchaAfter: failures within FailWindow that trigger a captcha challenge.
	CaptchaAfter int64
	// BlockAfter: failures within FailWindow that escalate to an aggressive,
	// longer-lived challenge (effectively a temporary block until solved).
	BlockAfter int64
	// ChallengeTTL is how long the stage-1 captcha challenge lasts.
	ChallengeTTL time.Duration
	// BlockTTL is how long the stage-2 aggressive (burst-window) block lasts —
	// roughly the remainder of FailWindow.
	BlockTTL time.Duration
	// DailyWindow is the longer rolling window for the per-subject daily cap.
	DailyWindow time.Duration
	// DailyBlockAfter: failures within DailyWindow that block the subject for the day.
	DailyBlockAfter int64
	// DailyBlockTTL is how long the daily block lasts — roughly the remainder of
	// DailyWindow.
	DailyBlockTTL time.Duration
	// GlobalWindow is the rolling window for merchant-wide failure counting.
	GlobalWindow time.Duration
	// GlobalAttackAfter: failures at one merchant within GlobalWindow that
	// put that merchant in attack mode, when they came from at least
	// GlobalAttackSubjects customers and as many client addresses. A few
	// accounts or addresses never make an attack: their own blocks contain them.
	GlobalAttackAfter    int64
	GlobalAttackSubjects int64
	// AttackTTL is how long the attack captcha stays up after the last
	// refusal seen while the merchant is under attack.
	AttackTTL time.Duration
}

// DefaultCardAbuseConfig returns the production policy.
func DefaultCardAbuseConfig() CardAbuseConfig {
	return CardAbuseConfig{
		FailWindow:           15 * time.Minute,
		CaptchaAfter:         3,
		BlockAfter:           6,
		ChallengeTTL:         15 * time.Minute,
		BlockTTL:             15 * time.Minute,
		DailyWindow:          24 * time.Hour,
		DailyBlockAfter:      10,
		DailyBlockTTL:        24 * time.Hour,
		GlobalWindow:         24 * time.Hour,
		GlobalAttackAfter:    100,
		GlobalAttackSubjects: 25,
		AttackTTL:            time.Hour,
	}
}

// CardAbuseGuard is the captcha accelerator over the FailureLedger: it
// captchas abusive subjects and, while the ledger reports an attack, everyone
// on that merchant's card routes. It is built only when a captcha is
// configured; otherwise the ledger's blocks are the whole policy.
type CardAbuseGuard struct {
	state      *abusestate.Store
	challenges *captcha.ChallengeStore
	cfg        CardAbuseConfig
}

// NewCardAbuseGuard builds the guard; a nil state or challenges makes it a
// no-op. cfg is used verbatim, with no zero-value defaulting.
func NewCardAbuseGuard(state *abusestate.Store, challenges *captcha.ChallengeStore, cfg CardAbuseConfig) *CardAbuseGuard {
	return &CardAbuseGuard{state: state, challenges: challenges, cfg: cfg}
}

func (g *CardAbuseGuard) enabled() bool {
	return g != nil && g.state != nil && g.challenges != nil
}

// countFailure counts one failure in key's window of unit and returns the
// window's count; unit keeps co-existing windows' keys apart.
func (g *CardAbuseGuard) countFailure(ctx context.Context, key, unit string, window time.Duration) int64 {
	count, _ := g.state.Count(ctx, "card_fail:"+key+":"+unit, 1, window)
	return count
}

// RecordChargeFailure counts one failed card attempt for each captcha subject
// (middleware.SubjectKeysFromContext, the subjects RateLimitHTTP pinned) and
// escalates it. attack, the ledger's verdict, (re)opens the merchant's attack
// captcha for AttackTTL.
func (g *CardAbuseGuard) RecordChargeFailure(ctx context.Context, merchantID uuid.UUID, subjectKeys []string, attack bool) {
	if !g.enabled() {
		return
	}
	for _, key := range subjectKeys {
		if key == "" {
			continue
		}

		// Daily window first, so it keeps counting after the burst window blocks.
		if daily := g.countFailure(ctx, key, "fail_day", g.cfg.DailyWindow); daily >= g.cfg.DailyBlockAfter {
			g.challenges.MarkChallenged(ctx, key, g.cfg.DailyBlockTTL)
			log.WithFields(log.Fields{"subject": key, "failures": daily}).Warn("card-abuse: subject blocked for the day after repeated card failures")
		}

		// Burst window (15 min): CaptchaAfter -> captcha, BlockAfter -> block for
		// the remainder of the window.
		count := g.countFailure(ctx, key, "fail", g.cfg.FailWindow)
		switch {
		case count >= g.cfg.BlockAfter:
			g.challenges.MarkChallenged(ctx, key, g.cfg.BlockTTL)
			log.WithFields(log.Fields{"subject": key, "failures": count}).Warn("card-abuse: subject blocked (aggressive captcha) after repeated card failures")
		case count >= g.cfg.CaptchaAfter:
			g.challenges.MarkChallenged(ctx, key, g.cfg.ChallengeTTL)
			log.WithFields(log.Fields{"subject": key, "failures": count}).Info("card-abuse: subject captcha-challenged after repeated card failures")
		}
	}

	if !attack || merchantID == uuid.Nil {
		return
	}
	g.challenges.MarkChallenged(ctx, captcha.CardAttackModeSubject(merchantID), g.cfg.AttackTTL)
	log.WithField("merchant_id", merchantID).Warn("card-abuse: attack mode — captcha required on this merchant's card routes")
}
