package openrails

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
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

// Embedded-only operations cannot appear on a remote Client's method set.
func TestLocalOperationsAreNotRemoteMethods(t *testing.T) {
	common := reflect.TypeFor[*Client]()
	local := reflect.TypeFor[*LocalClient]()
	for name := range embeddedMethods {
		_, remoteHas := common.MethodByName(name)
		require.False(t, remoteHas, name)
		_, localHas := local.MethodByName(name)
		require.True(t, localHas, name)
	}
	require.Equal(t, common.NumMethod()+len(embeddedMethods), local.NumMethod())
}

func TestLocalMerchantRequiresAnEngine(t *testing.T) {
	local, err := (&Engine{}).LocalMerchant(billing.MerchantID{})
	require.Error(t, err)
	require.Nil(t, local)
}

// Constructing a local extension does no I/O and keeps the shared handle and
// internal tooling bound to the same engine and merchant.
func TestLocalMerchantPreservesEngineAndMerchant(t *testing.T) {
	core := &engine.Engine{}
	bill := &Engine{engine: core}
	local, err := bill.LocalMerchant(testMerchant)
	require.NoError(t, err)
	require.Equal(t, testMerchant, local.MerchantID())
	require.Same(t, core, engine.Of(local))
	require.Same(t, core, engine.Of(local.Client))
	require.Nil(t, engine.Of(&LocalClient{}))
	_, err = bill.LocalMerchant(billing.MerchantID{})
	require.ErrorIs(t, err, billing.ErrInvalid)
}

// Merchant refuses the zero ID before anything is read.
func TestEngineMerchantRefusesTheZeroID(t *testing.T) {
	handle, err := (&Engine{}).Merchant(billing.MerchantID{})
	require.ErrorIs(t, err, billing.ErrInvalid)
	require.Nil(t, handle)
}
