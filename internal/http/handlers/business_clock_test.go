package handlers

import (
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/app"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSelfUsageWindowUsesRuntimeClock(t *testing.T) {
	now := time.Date(2040, 5, 15, 12, 30, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(now)
	req := httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/usage", nil), &app.Runtime{Clock: clock})
	from, to, ok := selfUsageWindow(req)
	require.True(t, ok)
	require.Equal(t, now, to)
	require.Equal(t, now.AddDate(0, -1, 0), from)
	clock.Advance(24 * time.Hour)
	_, to, ok = selfUsageWindow(req)
	require.True(t, ok)
	require.Equal(t, now.Add(24*time.Hour), to)

	req = httprequest.NewHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/usage?from=2030-01-01&to=2030-02-01", nil), &app.Runtime{Clock: clock})
	from, to, ok = selfUsageWindow(req)
	require.True(t, ok)
	require.Equal(t, 2030, from.Year())
	require.Equal(t, time.February, to.Month())
}
