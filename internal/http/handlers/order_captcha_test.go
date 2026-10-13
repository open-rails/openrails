package handlers

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
)

func TestOrderCardDeclineConcurrentObservationsCountOnce(t *testing.T) {
	state := abusestate.New(nil)
	challenges := captcha.NewChallengeStore(state)
	rt := &app.Runtime{AbuseState: state, CardAbuseGuard: abuse.NewCardAbuseGuard(state, challenges, abuse.DefaultCardAbuseConfig())}
	r, _ := newTestRequest(http.MethodPost, "/", nil, rt)
	ctx := middleware.WithSubjectKeys(merchant.WithID(t.Context(), billing.MerchantID(uuid.New())), []string{"ip:198.51.100.53"})
	r.Request = r.Request.WithContext(ctx)
	decline := uuid.New()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { recordOrderCardDecline(r, decline) })
	}
	wg.Wait()
	count, _ := state.Count(ctx, "card_fail:ip:198.51.100.53:fail", 0, 15*time.Minute)
	require.EqualValues(t, 1, count)
	require.False(t, challenges.IsChallenged(ctx, "ip:198.51.100.53"))
	for range 2 {
		recordOrderCardDecline(r, uuid.New())
	}
	require.True(t, challenges.IsChallenged(ctx, "ip:198.51.100.53"))
}
