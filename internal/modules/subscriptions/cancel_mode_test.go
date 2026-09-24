package subscriptions

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

type subOpt func(*models.Subscription)

func sub(rail models.Rail, status models.SubscriptionStatus, opts ...subOpt) *models.Subscription {
	s := &models.Subscription{ID: uuid.New(), CustomerID: uuid.New(), Rail: rail, Status: status}
	for _, o := range opts {
		o(s)
	}
	return s
}

func endsAt(t time.Time) subOpt   { return func(s *models.Subscription) { s.CurrentPeriodEndsAt = &t } }
func deleteAt(t time.Time) subOpt { return func(s *models.Subscription) { s.DeletionScheduledAt = &t } }
func cancelType(c models.CancelType) subOpt {
	return func(s *models.Subscription) { s.CancelType = &c }
}
func engineOwned(withMethod bool) subOpt {
	return func(s *models.Subscription) {
		s.CollectionPolicy = models.CollectionPolicyEngine
		if withMethod {
			pm := uuid.New()
			s.PaymentMethodID = &pm
		}
	}
}

// Resumable is the single gate for the handler, worker and DTO: reversible
// rail AND cancelled AND paid period still open; engine subscriptions only
// undo an ordinary user cancel that still has a card.
func TestCancelModeAndResumable(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	future, past := now.Add(10*24*time.Hour), now.Add(-time.Hour)
	pending := deleteAt(future.Add(-NMIDeleteSafetyMargin))
	for _, tc := range []struct {
		name      string
		sub       *models.Subscription
		mode      CancelMode
		resumable bool
		scheduled bool
	}{
		{"nil", nil, CancelModeDestructive, false, false},
		{"stripe active", sub(models.RailStripe, models.StatusActive, endsAt(future)), CancelModeReversible, false, false},
		{"stripe cancelled mid-period", sub(models.RailStripe, models.StatusCancelled, endsAt(future)), CancelModeReversible, true, true},
		{"stripe cancelled period over", sub(models.RailStripe, models.StatusCancelled, endsAt(past)), CancelModeReversible, false, false},
		{"stripe cancelled period unknown", sub(models.RailStripe, models.StatusCancelled), CancelModeReversible, false, false},
		{"ccbill cancelled (#696 no resume)", sub(models.RailCCBill, models.StatusCancelled, endsAt(future)), CancelModeDestructive, false, true},
		{"solana", sub(models.RailSolana, models.StatusCancelled, endsAt(future)), CancelModeDestructive, false, true},
		{"nmi active", sub(models.RailNMI, models.StatusActive, endsAt(future)), CancelModeDestructive, false, false},
		{"nmi delete pending", sub(models.RailNMI, models.StatusCancelled, endsAt(future), pending), CancelModeReversible, true, true},
		{"nmi delete executed", sub(models.RailNMI, models.StatusCancelled, endsAt(future)), CancelModeDestructive, false, true},
		{"nmi delete pending, period over", sub(models.RailNMI, models.StatusCancelled, endsAt(past), pending), CancelModeDestructive, false, false},
		{"engine user cancel", sub(models.RailNMI, models.StatusCancelled, endsAt(future), engineOwned(true), cancelType(models.CancelTypeUser)), CancelModeReversible, true, true},
		{"engine user cancel without card", sub(models.RailNMI, models.StatusCancelled, endsAt(future), engineOwned(false), cancelType(models.CancelTypeUser)), CancelModeReversible, false, true},
		{"engine merchant cancel", sub(models.RailStripe, models.StatusCancelled, endsAt(future), engineOwned(true), cancelType(models.CancelTypeMerchant)), CancelModeReversible, false, true},
		{"engine chargeback", sub(models.RailStripe, models.StatusCancelled, endsAt(future), engineOwned(true), cancelType(models.CancelTypeChargeback)), CancelModeReversible, false, true},
		{"engine cancel type unknown", sub(models.RailStripe, models.StatusCancelled, endsAt(future), engineOwned(true)), CancelModeReversible, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.mode, CancelModeFor(tc.sub, now))
			require.Equal(t, tc.resumable, Resumable(tc.sub, now))
			require.Equal(t, tc.scheduled, CancelScheduled(tc.sub, now))
			// #696: no rail sends the customer to an external portal.
			if tc.sub != nil {
				require.Nil(t, CancelPortalURL(tc.sub, now))
			}
		})
	}
}

// The NMI delete must land strictly before the rebill at period end: a user
// cancel defers to period_end-48h only when that instant is still ahead.
func TestNMIDeferredDeleteAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		sub       *models.Subscription
		wantDefer bool
	}{
		{"nil", nil, false},
		{"unknown period end", sub(models.RailNMI, models.StatusActive), false},
		{"past period end", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(-time.Hour))), false},
		{"inside margin", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(12*time.Hour))), false},
		{"exactly at margin", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(NMIDeleteSafetyMargin))), false},
		{"one ns beyond margin", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(NMIDeleteSafetyMargin+1))), true},
		{"far out", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(10*24*time.Hour))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, deferred := NMIDeferredDeleteAt(tc.sub, now)
			require.Equal(t, tc.wantDefer, deferred)
			if deferred {
				require.Equal(t, tc.sub.CurrentPeriodEndsAt.Add(-NMIDeleteSafetyMargin), at)
				require.True(t, at.After(now))
			} else {
				require.True(t, at.IsZero())
			}
		})
	}
}

// or#842: automated terminal cancels get a cooling-off window, clamped so the
// delete still precedes a known rebill; with no safe room it is due now.
func TestSystemDeferredDeleteAt(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	full := now.Add(SystemDeleteCoolingOff)
	clamped := now.Add(NMIDeleteSafetyMargin + 6*time.Hour)
	for _, tc := range []struct {
		name string
		sub  *models.Subscription
		want time.Time
	}{
		{"lapsed period", sub(models.RailNMI, models.StatusPastDue, endsAt(now.Add(-30*24*time.Hour))), full},
		{"unknown period end", sub(models.RailNMI, models.StatusPastDue), full},
		{"distant rebill", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(10*24*time.Hour))), full},
		{"rebill inside window", sub(models.RailNMI, models.StatusActive, endsAt(clamped)), clamped.Add(-NMIDeleteSafetyMargin)},
		{"margin already open", sub(models.RailNMI, models.StatusActive, endsAt(now.Add(12*time.Hour))), now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SystemDeferredDeleteAt(tc.sub, now)
			require.Equal(t, tc.want, got)
			require.False(t, got.Before(now))
			if end := tc.sub.CurrentPeriodEndsAt; end != nil && end.After(now) {
				require.True(t, got.Before(*end), "delete must precede the rebill")
			}
		})
	}
}
