package openrails

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// River decodes a job by kind into the worker's args type, so the public args
// must mirror the engine job field for field or host inserts silently drop data.
func TestInvoiceSweepArgsMirrorEngineInvoiceJob(t *testing.T) {
	public, internal := reflect.TypeOf(InvoiceSweepArgs{}), reflect.TypeOf(riverjobs.InvoiceArgs{})
	require.Equal(t, internal.NumField(), public.NumField())
	for i := range internal.NumField() {
		want := internal.Field(i)
		got, ok := public.FieldByName(want.Name)
		require.True(t, ok, want.Name)
		require.Equal(t, want.Type, got.Type, want.Name)
		require.Equal(t, want.Tag, got.Tag, want.Name)
	}
	require.Equal(t, riverjobs.InvoiceArgs{}.Kind(), InvoiceSweepArgs{}.Kind())
	require.Equal(t, QueueBilling, InvoiceSweepArgs{}.InsertOpts().Queue)
}

// Hosting operations refuse a remote client instead of pretending.
func TestRemoteClientRefusesHostingOperations(t *testing.T) {
	c, err := NewRemote("https://billing.example", WithAPIKey("k"))
	require.NoError(t, err)
	require.ErrorIs(t, c.Start(t.Context()), ErrRemoteClient)
	_, err = c.Routes(Routes{Prefix: "/billing"})
	require.ErrorIs(t, err, ErrRemoteClient)
	require.Nil(t, c.Probes())
	_, err = c.DeclarePSP(t.Context(), billing.MerchantID(uuid.New()), billing.PSPDeclaration{})
	require.ErrorIs(t, err, ErrRemoteClient)
	require.NoError(t, c.Close(t.Context()))
}

// New authenticates as the host over its own transport: the options that
// configure a remote client's credential or transport are refused before
// anything opens, never dropped.
func TestNewRefusesRemoteOptions(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	for name, opt := range map[string]ClientOption{
		"WithAPIKey":             WithAPIKey("k"),
		"WithTokenProvider":      WithTokenProvider(token),
		"WithHTTPClient":         WithHTTPClient(&http.Client{}),
		"WithCredentialProvider": WithCredentialProvider(func(context.Context, CredentialTarget) (string, error) { return "t", nil }),
	} {
		_, err := New(t.Context(), Config{}, Deps{}, opt)
		require.ErrorContains(t, err, "New runs in process", name)
	}
	_, err := New(t.Context(), Config{}, Deps{}, WithMerchantID(billing.MerchantID{}))
	require.ErrorContains(t, err, "merchant ID must not be zero", "an invalid option fails before the engine is built")
}

// Only the Client a constructor returned owns the engine and transport.
func TestDerivedClientCannotClose(t *testing.T) {
	c, err := NewRemote("https://billing.example", WithAPIKey("k"))
	require.NoError(t, err)
	derived, err := c.With(WithTimeout(1))
	require.NoError(t, err)
	again, err := derived.With()
	require.NoError(t, err)
	for _, d := range []*Client{derived, again} {
		require.ErrorContains(t, d.Close(t.Context()), "derived client")
	}
	require.NoError(t, c.Close(t.Context()))
}
