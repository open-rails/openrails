package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Only an executor claim its heartbeat still holds may charge; these refusals
// happen before any read.
func TestRequireClaimRefusesWithoutAnExecutorClaim(t *testing.T) {
	id := uuid.New()
	s := &Store{}
	require.ErrorIs(t, s.RequireClaim(context.Background(), id, time.Now()), ErrClaimLost, "no claim")

	verify, _ := withClaim(context.Background(), gen.OpenrailsRailIntent{ID: id, Status: StatusUnknownNeedsVerify, Attempts: 3})
	require.ErrorIs(t, s.RequireClaim(verify, id, time.Now()), ErrClaimLost, "a verification claim never charges")

	exec, held := withClaim(context.Background(), gen.OpenrailsRailIntent{ID: id, Status: StatusInFlight, Attempts: 3})
	require.ErrorIs(t, s.RequireClaim(exec, uuid.New(), time.Now()), ErrClaimLost, "another operation")
	held.lose()
	require.ErrorIs(t, s.RequireClaim(exec, id, time.Now()), ErrClaimLost, "the heartbeat lost the claim")
}
