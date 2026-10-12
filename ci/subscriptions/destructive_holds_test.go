//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/stretchr/testify/require"
)

type incidentMailer struct {
	mu       sync.Mutex
	attempts int
	messages []openrails.Email
}

func (m *incidentMailer) CheckHealth(context.Context) error { return nil }
func (m *incidentMailer) Send(_ context.Context, email openrails.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts++
	if m.attempts == 1 {
		return errors.New("fixture mail outage")
	}
	m.messages = append(m.messages, email)
	return nil
}

func TestDestructiveIncidentProjectsAndRetriesAlert(t *testing.T) {
	w := prepareWorld(t, 4)
	w.riverWorkers = 2
	mailbox := &incidentMailer{}
	address := "operator@example.test"
	w.settings.AlertEmail = &address
	w.deps = func(deps *openrails.Deps) { deps.Email = mailbox }
	w.start()
	ctx := t.Context()
	mid := w.client[embedded].MerchantID()
	gate := engine.Graph(w.rt).Runtime.DestructiveAdmissions
	actor := destructive.Actor{Issuer: "https://staff.example", ID: "compromised", Credential: "session:test"}
	command := func(n int) destructive.Command {
		key := fmt.Sprintf("fixture-refund-%d", n)
		return destructive.Command{MerchantID: mid.UUID(), Kind: destructive.Refund, Target: key, Key: key, Digest: destructive.Digest(key), Units: 1, Actor: actor}
	}
	for i := range 5 {
		a, err := gate.Admit(ctx, command(i))
		require.NoError(t, err)
		_, err = gate.Start(ctx, mid.UUID(), a.ID)
		require.NoError(t, err)
		require.NoError(t, gate.Finish(ctx, mid.UUID(), a.ID, destructive.OutcomeSucceeded, "fixture-completed"))
	}
	_, err := gate.Admit(ctx, command(5))
	var trip *destructive.HeldError
	require.ErrorAs(t, err, &trip)
	require.Eventually(t, func() bool { mailbox.mu.Lock(); defer mailbox.mu.Unlock(); return len(mailbox.messages) == 1 }, 30*time.Second, 50*time.Millisecond, "durable alert retries a failed sender")
	mailbox.mu.Lock()
	require.GreaterOrEqual(t, mailbox.attempts, 2)
	require.Contains(t, mailbox.messages[0].Text, "held")
	mailbox.mu.Unlock()
	findings, err := w.client[embedded].ListFindings(ctx, billing.FindingListParams{Type: "life.destructive.hold"})
	require.NoError(t, err)
	require.Len(t, findings.Items, 1)
	_, err = w.client[embedded].ResolveFinding(ctx, findings.Items[0].ID, billing.ResolveFindingParams{Outcome: billing.FindingIgnore, Notes: "cannot clear through ignore"})
	require.Error(t, err)
	events, err := w.client[embedded].ListHostEvents(ctx, billing.HostEventListParams{Type: billing.HostEventDestructiveHoldOpened})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	require.Equal(t, billing.DestructiveHoldID(trip.Hold.ID), events.Items[0].DestructiveHold.HoldID)
	// A self-acting host application has all scope permissions but is not an
	// interactive human authorized to clear an incident over the merchant API.
	_, err = w.client[embedded].ClearDestructiveHold(ctx, billing.DestructiveHoldID(trip.Hold.ID), billing.ClearDestructiveHoldParams{Reason: "application refused"})
	require.Error(t, err)
	w.restart()
	gate = engine.Graph(w.rt).Runtime.DestructiveAdmissions
	_, err = gate.Admit(ctx, command(6))
	require.ErrorIs(t, err, destructive.ErrHeld)
	_, err = w.rt.ClearDestructiveHold(ctx, mid, billing.DestructiveHoldID(trip.Hold.ID), billing.DestructiveOperator{Issuer: actor.Issuer, ID: actor.ID}, "self refused")
	require.ErrorIs(t, err, destructive.ErrSelfClear)
	cleared, err := w.rt.ClearDestructiveHold(ctx, mid, billing.DestructiveHoldID(trip.Hold.ID), billing.DestructiveOperator{Issuer: "https://operators.example", ID: "reviewer"}, "credential revoked and reviewed")
	require.NoError(t, err)
	require.NotNil(t, cleared.ClearedAt)
	events, err = w.client[embedded].ListHostEvents(ctx, billing.HostEventListParams{Type: billing.HostEventDestructiveHoldCleared})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	_, err = gate.Admit(ctx, command(7))
	require.NoError(t, err)
}
