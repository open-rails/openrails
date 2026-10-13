package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/abuse"
	billingservice "github.com/open-rails/openrails/internal/service"
)

func TestOrderResponseRefreshesMerchantAttackChallenge(t *testing.T) {
	state := abusestate.New(nil)
	challenges := captcha.NewChallengeStore(state)
	cfg := abuse.DefaultCardAbuseConfig()
	cfg.GlobalAttackAfter, cfg.GlobalAttackSubjects = 1, 1
	ledger := abuse.NewFailureLedger(state, clockwork.NewRealClock(), cfg)
	rt := &app.Runtime{CardFailureLedger: ledger, CardAbuseGuard: abuse.NewCardAbuseGuard(state, challenges, cfg)}
	r, _ := newTestRequest(http.MethodPost, "/", nil, rt)
	mid := billing.MerchantID(uuid.New())
	ctx := merchant.WithID(t.Context(), mid)
	r.Request = r.Request.WithContext(ctx)
	// The inline caller records its blocking failure after the runner returns.
	ledger.Record(ctx, mid.UUID(), abuse.CustomerSubject(uuid.NewString()), "ip:198.51.100.53", abuse.MerchantSubject)
	answerOrder(r, billingservice.OrderAnswer{Order: &billing.Order{}, Status: http.StatusPaymentRequired, Code: billing.CodeCardDeclined})
	require.True(t, challenges.IsChallenged(ctx, captcha.CardAttackModeSubject(mid.UUID())))
	count, _ := state.Count(ctx, "card_fail:ip:198.51.100.53:fail", 0, 15*time.Minute)
	require.Zero(t, count, "refreshing attack mode must not add another payment failure")
}
