package abuse

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/captcha"
)

func TestOrderCardDeclineConcurrentObservationsCountOnce(t *testing.T) {
	state := abusestate.New(nil)
	challenges := captcha.NewChallengeStore(state)
	guard := NewCardAbuseGuard(state, challenges, DefaultCardAbuseConfig())
	ctx := t.Context()
	merchantID := uuid.New()
	subjects := []string{"ip:198.51.100.53"}
	decline := uuid.New()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { guard.RecordOrderDecline(ctx, merchantID, decline, subjects, false) })
	}
	wg.Wait()
	count, _ := state.Count(ctx, "card_fail:ip:198.51.100.53:fail", 0, 15*time.Minute)
	require.EqualValues(t, 1, count)
	require.False(t, challenges.IsChallenged(ctx, "ip:198.51.100.53"))
	for range 2 {
		guard.RecordOrderDecline(ctx, merchantID, uuid.New(), subjects, false)
	}
	require.True(t, challenges.IsChallenged(ctx, "ip:198.51.100.53"))
}
