package embed

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestEngineContextKeepsOnlyCancellation(t *testing.T) {
	type hostKey struct{}
	deadline := time.Now().Add(time.Hour)
	host, cancel := context.WithDeadline(context.WithValue(context.Background(), hostKey{}, "host"), deadline)
	defer cancel()
	host = billingauth.SetUserContext(merchant.WithID(host, merchant.ID(uuid.New())), billingauth.UserContext{UserID: uuid.NewString()})

	ctx := engineContext(host)
	require.Nil(t, ctx.Value(hostKey{}))
	_, ok := billingauth.FromContext(ctx)
	require.False(t, ok, "host session user must not reach the engine")
	_, ok = merchant.FromContext(ctx)
	require.False(t, ok, "host merchant pin must not reach the engine")

	got, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, got)

	bound := merchant.ID(uuid.New())
	engine := merchant.WithID(ctx, bound)
	pinned, ok := merchant.FromContext(engine)
	require.True(t, ok)
	require.Equal(t, bound, pinned)
	require.Nil(t, engine.Value(hostKey{}))

	derived, derivedCancel := context.WithCancel(engine)
	defer derivedCancel()
	require.NoError(t, derived.Err())
	cancel()
	<-derived.Done()
	require.ErrorIs(t, derived.Err(), context.Canceled)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
