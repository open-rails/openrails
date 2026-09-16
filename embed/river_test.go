package embed

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	riverjobs "github.com/open-rails/openrails/internal/river"
)

// The public args must be the engine job's args field for field: River decodes
// a job by kind into the worker's type, so a drifted tag or type would be
// silently dropped on the way in.
func TestInvoiceSweepArgsMirrorEngineInvoiceJob(t *testing.T) {
	public := reflect.TypeOf(InvoiceSweepArgs{})
	engine := reflect.TypeOf(riverjobs.InvoiceArgs{})
	require.Equal(t, engine.NumField(), public.NumField())
	for i := 0; i < engine.NumField(); i++ {
		want := engine.Field(i)
		got, ok := public.FieldByName(want.Name)
		require.True(t, ok, "field %s missing from InvoiceSweepArgs", want.Name)
		require.Equal(t, want.Type, got.Type, "field %s type", want.Name)
		require.Equal(t, want.Tag, got.Tag, "field %s tag", want.Name)
	}
	require.Equal(t, riverjobs.InvoiceArgs{}.Kind(), InvoiceSweepArgs{}.Kind())
	require.Equal(t, QueueBilling, InvoiceSweepArgs{}.InsertOpts().Queue)
}
