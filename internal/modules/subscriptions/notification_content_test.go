package subscriptions

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #789: access-ended mail goes to often long-lapsed users, so its copy stays
// neutral, and the CTA exists only when there is somewhere to send them.
func TestRenderAccessEndedEmail(t *testing.T) {
	endedAt := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	c := RenderAccessEndedEmail("Host One", "https://example.com/signup", "alice", endedAt)
	require.Equal(t, "Your Host One premium access has ended", c.Subject)
	for _, body := range []string{c.HTML, c.Plain} {
		require.Contains(t, body, "Hi alice,")
		require.Contains(t, body, "Jul 4, 2026")
		require.Contains(t, body, "https://example.com/signup")
		for _, banned := range []string{"charge", "payment", "renew"} {
			require.NotContains(t, strings.ToLower(body), banned)
		}
	}
	require.Contains(t, c.HTML, `href="https://example.com/signup"`)

	c = RenderAccessEndedEmail("Host One", "", "  ", endedAt)
	require.NotContains(t, c.HTML, "href=")
	require.NotContains(t, c.Plain, "Sign up again any time")
	require.Contains(t, c.HTML, "Hi there,")
}

func TestParsePremiumEndReasonRoundTrips(t *testing.T) {
	for _, r := range []PremiumEndReason{
		PremiumEndReasonUserCancel, PremiumEndReasonExpired, PremiumEndReasonChargeback, PremiumEndReasonRefund,
		PremiumEndReasonAdmin, PremiumEndReasonRail, PremiumEndReasonAccessEnded, PremiumEndReasonNonRecoverable,
	} {
		require.Equal(t, r, ParsePremiumEndReason(string(r)))
		require.Equal(t, r, ParsePremiumEndReason(strings.ToUpper(string(r))))
	}
	require.Equal(t, PremiumEndReasonUnknown, ParsePremiumEndReason("bogus"))
	require.Equal(t, PremiumEndReasonUnknown, ParsePremiumEndReason(""))
}
