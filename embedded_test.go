package openrails

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// River decodes a job by kind into the worker's args type, so the public args
// must mirror the engine job field for field or host inserts silently drop data.
func TestUsageSweepArgsMirrorEngineJob(t *testing.T) {
	public, internal := reflect.TypeOf(UsageSweepArgs{}), reflect.TypeOf(riverjobs.UsageOrderArgs{})
	require.Equal(t, internal.NumField(), public.NumField())
	for i := range internal.NumField() {
		want := internal.Field(i)
		got, ok := public.FieldByName(want.Name)
		require.True(t, ok, want.Name)
		require.Equal(t, want.Type, got.Type, want.Name)
		require.Equal(t, want.Tag, got.Tag, want.Name)
	}
	require.Equal(t, riverjobs.UsageOrderArgs{}.Kind(), UsageSweepArgs{}.Kind())
	require.Equal(t, QueueBilling, UsageSweepArgs{}.InsertOpts().Queue)
}

// What only an engine's handle offers refuses a remote handle instead of
// pretending.
func TestRemoteHandleRefusesEmbeddedOperations(t *testing.T) {
	c, err := NewRemoteMerchant("https://billing.example", RemoteCredential{APIKey: "k"})
	require.NoError(t, err)
	_, err = c.DeclarePSP(t.Context(), billing.PSPDeclaration{})
	require.ErrorIs(t, err, ErrRemoteClient)
	_, err = c.SCIMHandler()
	require.ErrorIs(t, err, ErrRemoteClient)
	_, err = c.StartRunningChargeTx(t.Context(), nil, billing.StartRunningChargeParams{})
	require.ErrorIs(t, err, ErrRemoteClient)
}

// Merchant refuses the zero ID before anything is read.
func TestEngineMerchantRefusesTheZeroID(t *testing.T) {
	handle, err := (&Engine{}).Merchant(billing.MerchantID{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.Nil(t, handle)
}
