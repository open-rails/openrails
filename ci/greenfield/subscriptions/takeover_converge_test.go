//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed/operator"
	"github.com/open-rails/openrails/internal/failpoint"
)

// A convergence pass that runs at any point of an NMI engine takeover (before
// the schedule delete, or after it and before the local commit) repairs
// nothing on the legacy membership, and the takeover still ends with legacy
// access bounded at the boundary.
func TestNMIEngineTakeoverConvergesMidFlight(t *testing.T) {
	t.Parallel()
	for _, point := range []failpoint.Point{failpoint.BeforeProvider, failpoint.BeforeComplete} {
		t.Run(string(point), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importTakeoverLegacy(t, w, embedded)
			end := l.periodEnd()
			legacy := l.sub.UUID()
			converged := 0
			remove := failpoint.Set(func(ctx context.Context, s failpoint.Site) error {
				if s.Point != point || s.Subscription != legacy || s.Kind != "nmi_engine_takeover" {
					return nil
				}
				res, err := operator.New(w.rt).Converge(context.Background(), w.client[embedded].MerchantID())
				if err != nil {
					t.Errorf("mid-takeover converge: %v", err)
				}
				t.Logf("mid-takeover converge at %s: %+v", point, res)
				converged++
				return nil
			})
			defer remove()
			done := l.takeover("takeover-" + uuid.NewString())
			require.Equal(t, "completed", done.Stage, "%+v", done)
			require.Equal(t, 1, converged, "convergence ran inside the takeover")
			require.Empty(t, w.findingsAbout(l.sub), "nothing for convergence to repair on the legacy membership mid-takeover")
			for _, win := range w.subscriptionWindows(l.sub) {
				require.NotNil(t, win.end, "the legacy window is bounded by the takeover")
				require.False(t, win.end.After(end))
			}
			require.True(t, l.c.entitled(l.ent))
		})
	}
}
