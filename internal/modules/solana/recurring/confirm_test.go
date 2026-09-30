package recurring

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

var zeroSig = solanago.Signature{}.String()

// watcher fakes the signature watch: landed-and-succeeded, landed-and-reverted,
// or never observed.
type watcher struct {
	outcome *solanaint.TransactionOutcome
	err     error
	calls   int
	comm    rpc.CommitmentType
}

func (w *watcher) WatchTransaction(_ context.Context, _ solanago.Signature, comm rpc.CommitmentType, _ solanaint.ChainTerminal) (*solanaint.TransactionOutcome, error) {
	w.calls++
	w.comm = comm
	return w.outcome, w.err
}

func landed() *watcher { return &watcher{outcome: &solanaint.TransactionOutcome{}} }
func reverted() *watcher {
	return &watcher{outcome: &solanaint.TransactionOutcome{Err: map[string]any{"InstructionError": []any{0, "Custom"}}}}
}
func unconfirmed() *watcher { return &watcher{err: errors.New("context deadline exceeded")} }

type canceller struct {
	params []*submod.CancelMembershipParams
}

func (c *canceller) CancelMembership(_ context.Context, p *submod.CancelMembershipParams) error {
	c.params = append(c.params, p)
	return nil
}

// The chain is the source of truth: only a confirmed-and-succeeded cancel
// mirrors, and it mirrors as a SCHEDULED period-end cancel, not a revoke.
func TestConfirmCancel(t *testing.T) {
	subID := uuid.New()
	for _, tc := range []struct {
		name   string
		w      *watcher
		subID  uuid.UUID
		sig    string
		mirror bool
	}{
		{"succeeded", landed(), subID, zeroSig, true},
		{"reverted", reverted(), subID, zeroSig, false},
		{"never confirmed", unconfirmed(), subID, zeroSig, false},
		{"invalid signature", landed(), subID, "not-base58!!", false},
		{"empty signature", landed(), subID, "", false},
		{"nil subscription", landed(), uuid.Nil, zeroSig, false},
	} {
		c := &canceller{}
		err := NewConfirmCancelService(tc.w, c).Confirm(context.Background(), tc.subID, tc.sig)
		if !tc.mirror {
			require.Error(t, err, tc.name)
			require.Empty(t, c.params, tc.name)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, rpc.CommitmentConfirmed, tc.w.comm)
		require.Len(t, c.params, 1)
		require.Equal(t, subID, *c.params[0].SubscriptionID)
		require.False(t, c.params[0].RevokeAccess, "cancel at period end")
		require.Equal(t, models.CancelTypeUser, c.params[0].CancelType)
	}
}
