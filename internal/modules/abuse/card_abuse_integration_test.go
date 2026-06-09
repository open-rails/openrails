//go:build integration

package abuse_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func newCardAbuse(t *testing.T, cfg abuse.CardAbuseConfig) (*abuse.CardAbuseGuard, *captcha.ChallengeStore, context.Context) {
	t.Helper()
	guard, challenges, _, ctx := newCardAbuseWithClock(t, cfg)
	return guard, challenges, ctx
}

// newCardAbuseWithClock is like newCardAbuse but also returns the underlying
// limiter so a test can drive its clock (to make a short window roll while a
// longer window keeps accruing).
func newCardAbuseWithClock(t *testing.T, cfg abuse.CardAbuseConfig) (*abuse.CardAbuseGuard, *captcha.ChallengeStore, *ratelimit.Limiter, context.Context) {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	conn, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	challenges := captcha.NewChallengeStore(nil) // memory-backed challenge store
	lim := ratelimit.NewLimiter(rdb)
	guard := abuse.NewCardAbuseGuard(lim, challenges, cfg)
	return guard, challenges, lim, ctx
}

func TestCardAbuse_PerSubjectEscalatesToCaptcha(t *testing.T) {
	guard, challenges, ctx := newCardAbuse(t, abuse.CardAbuseConfig{
		FailWindow: time.Minute, CaptchaAfter: 3, BlockAfter: 5,
		ChallengeTTL: time.Minute, BlockTTL: time.Minute,
		GlobalWindow: time.Hour, GlobalAttackAfter: 1000, AttackTTL: time.Minute,
	})
	ip := "ip:" + uuid.NewString()

	// Below threshold: no captcha for a couple of failures.
	for i := 0; i < 2; i++ {
		guard.RecordChargeFailure(ctx, []string{ip})
	}
	challenged, err := challenges.IsChallenged(ctx, ip)
	require.NoError(t, err)
	require.False(t, challenged, "2 failures must not challenge yet")

	// 3rd failure crosses CaptchaAfter -> challenged.
	guard.RecordChargeFailure(ctx, []string{ip})
	challenged, err = challenges.IsChallenged(ctx, ip)
	require.NoError(t, err)
	require.True(t, challenged, "3 failures must captcha-challenge the subject")

	// An unrelated subject is not challenged.
	other, err := challenges.IsChallenged(ctx, "ip:"+uuid.NewString())
	require.NoError(t, err)
	require.False(t, other, "an unrelated subject must never be challenged")
}

func TestCardAbuse_FifteenMinWindowCaptchaThenBlock(t *testing.T) {
	// Default policy: 15-min window, 3 -> captcha, 6 -> block.
	guard, challenges, ctx := newCardAbuse(t, abuse.DefaultCardAbuseConfig())
	subj := "user:" + uuid.NewString()

	for i := 0; i < 2; i++ {
		guard.RecordChargeFailure(ctx, []string{subj})
	}
	challenged, err := challenges.IsChallenged(ctx, subj)
	require.NoError(t, err)
	require.False(t, challenged, "2 failures must not challenge yet")

	// 3rd failure within 15 min -> captcha challenge.
	guard.RecordChargeFailure(ctx, []string{subj})
	challenged, err = challenges.IsChallenged(ctx, subj)
	require.NoError(t, err)
	require.True(t, challenged, "3 failures in 15 min must captcha-challenge")

	// Failures 4..6 -> escalate to a block (BlockTTL ~ remainder of window). We
	// can't observe the TTL distinction here, but the subject stays challenged.
	for i := 0; i < 3; i++ {
		guard.RecordChargeFailure(ctx, []string{subj})
	}
	challenged, err = challenges.IsChallenged(ctx, subj)
	require.NoError(t, err)
	require.True(t, challenged, "6 failures in 15 min must keep the subject blocked")
}

func TestCardAbuse_DailyCapBlocksWhenBurstWindowResets(t *testing.T) {
	// Short burst window with a high block threshold so paced failures never trip
	// it, plus a 24h daily cap of 10. We advance the limiter clock past the burst
	// window between failures so the burst counter rolls (staying at 1) while the
	// daily counter accrues to 10.
	cfg := abuse.CardAbuseConfig{
		FailWindow: time.Minute, CaptchaAfter: 3, BlockAfter: 6,
		ChallengeTTL: time.Minute, BlockTTL: time.Minute,
		DailyWindow: 24 * time.Hour, DailyBlockAfter: 10, DailyBlockTTL: 24 * time.Hour,
		GlobalWindow: 24 * time.Hour, GlobalAttackAfter: 1000, AttackTTL: time.Minute,
	}
	guard, challenges, lim, ctx := newCardAbuseWithClock(t, cfg)
	subj := "user:" + uuid.NewString()

	// Pin to mid-day UTC so the 18 minutes of paced failures never straddle a UTC
	// midnight (which would roll the 24h daily bucket).
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	// 9 paced failures: each in a fresh burst bucket (2-min apart > 1-min window),
	// so the burst counter never reaches CaptchaAfter and never challenges.
	for i := 0; i < 9; i++ {
		now := base.Add(time.Duration(i) * 2 * time.Minute)
		lim.SetClock(func() time.Time { return now })
		guard.RecordChargeFailure(ctx, []string{subj})

		challenged, err := challenges.IsChallenged(ctx, subj)
		require.NoError(t, err)
		require.Falsef(t, challenged, "must not be challenged after %d paced failures (burst window keeps resetting)", i+1)
	}

	// 10th failure (still inside the 24h window) trips the daily cap.
	now := base.Add(9 * 2 * time.Minute)
	lim.SetClock(func() time.Time { return now })
	guard.RecordChargeFailure(ctx, []string{subj})

	challenged, err := challenges.IsChallenged(ctx, subj)
	require.NoError(t, err)
	require.True(t, challenged, "10 failures across the day must block via the daily cap")
}

func TestCardAbuse_NoFailuresNeverChallenges(t *testing.T) {
	_, challenges, ctx := newCardAbuse(t, abuse.DefaultCardAbuseConfig())
	challenged, err := challenges.IsChallenged(ctx, "user:"+uuid.NewString())
	require.NoError(t, err)
	require.False(t, challenged, "a normal user must never see a captcha")
}

func TestCardAbuse_SiteWideAttackMode(t *testing.T) {
	guard, _, ctx := newCardAbuse(t, abuse.CardAbuseConfig{
		FailWindow: time.Minute, CaptchaAfter: 100, BlockAfter: 200,
		ChallengeTTL: time.Minute, BlockTTL: time.Minute,
		GlobalWindow: time.Hour, GlobalAttackAfter: 5, AttackTTL: time.Minute,
	})

	active, err := guard.AttackModeActive(ctx)
	require.NoError(t, err)
	require.False(t, active, "attack mode off before any failures")

	// Each call increments the global counter once (distinct subjects so no
	// per-subject captcha noise). Cross GlobalAttackAfter=5.
	for i := 0; i < 5; i++ {
		guard.RecordChargeFailure(ctx, []string{"ip:" + uuid.NewString()})
	}
	active, err = guard.AttackModeActive(ctx)
	require.NoError(t, err)
	require.True(t, active, "site-wide attack mode must engage after the global threshold")
}

func TestCardAbuse_NilGuardIsNoOp(t *testing.T) {
	var guard *abuse.CardAbuseGuard
	require.NotPanics(t, func() {
		guard.RecordChargeFailure(context.Background(), []string{"ip:x"})
	})
	active, err := guard.AttackModeActive(context.Background())
	require.NoError(t, err)
	require.False(t, active)
}
