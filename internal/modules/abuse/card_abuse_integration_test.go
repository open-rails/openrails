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
	guard := abuse.NewCardAbuseGuard(ratelimit.NewLimiter(rdb), challenges, cfg)
	return guard, challenges, ctx
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
