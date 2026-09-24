package openrails

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/pkg/merchant"
)

// archiveFixture exercises framing without the engine's schema.
func archiveFixture(t *testing.T, mid MerchantID, customers int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := archivewire.NewWriter(&buf, mid.String())
	require.NoError(t, err)
	require.NoError(t, w.Table("customers"))
	merchantID, issuer, timestamp := mid.String(), "https://merchant.example/auth", "2026-09-17 12:00:00+00"
	for i := range customers {
		id := uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprint(i))).String()
		require.NoError(t, w.Row([]*string{&merchantID, &id, &issuer, &timestamp, &timestamp}))
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// A real book exceeds the 1 MiB JSON cap; both directions stream it with the
// same credential, merchant assertion and selector as every other operation.
func TestMerchantArchiveStreamsBothDirections(t *testing.T) {
	mid := MerchantID(uuid.New())
	archive := archiveFixture(t, mid, 7000)
	require.Greater(t, len(archive), 1<<20)
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, merchantBillingArchivePath, r.URL.Path)
		require.Equal(t, "Bearer id:"+mid.String(), r.Header.Get("Authorization"))
		require.Equal(t, mid.String(), r.Header.Get(merchant.BindingHeader))
		require.Empty(t, r.Header.Get(merchant.SlugHeader))
		if r.Method == http.MethodGet {
			require.Equal(t, "application/x-ndjson", r.Header.Get("Accept"))
			_, _ = w.Write(archive)
			return
		}
		require.Equal(t, "application/x-ndjson", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.True(t, bytes.Equal(archive, body), "uploaded archive changed or was truncated")
		_, _ = fmt.Fprintf(w, `{"merchant_id":%q,"digest":"receipt","rows":"7000","already_imported":false}`, mid.String())
	}, WithCredentialProvider(targetCredential))
	var dst bytes.Buffer
	require.NoError(t, client.ExportMerchantBilling(t.Context(), &dst, ForMerchantID(mid)))
	require.True(t, bytes.Equal(archive, dst.Bytes()))
	result, err := client.ImportMerchantBilling(t.Context(), bytes.NewReader(dst.Bytes()), ForMerchantID(mid))
	require.NoError(t, err)
	require.Equal(t, MerchantBillingImportResult{MerchantID: mid, Digest: "receipt", Rows: 7000}, *result)
	require.Equal(t, "fixture", client.merchantSlug, "per-call selection leaves the default")
}

func TestMerchantArchiveRefusals(t *testing.T) {
	mid := MerchantID(uuid.New())
	archive := archiveFixture(t, mid, 1)
	var calls atomic.Int64
	var handler atomic.Value
	reply := func(h http.HandlerFunc) { handler.Store(h) }
	client := newTestRemote(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler.Load().(http.HandlerFunc)(w, r)
	}, WithMerchantID(mid))

	reply(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive[:len(archive)-8]) })
	require.ErrorIs(t, client.ExportMerchantBilling(t.Context(), io.Discard), ErrUnreachable, "a truncated success is not an archive")

	reply(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-ID", "archive-request")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"billing_archive_not_empty","message":"destination billing state must be empty"}}`))
	})
	_, err := client.ImportMerchantBilling(t.Context(), bytes.NewReader(archive))
	var status *StatusError
	require.ErrorAs(t, err, &status)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, "billing_archive_not_empty", status.Code)
	require.Equal(t, "archive-request", status.RequestID)
	require.ErrorAs(t, client.ExportMerchantBilling(t.Context(), io.Discard), &status, "export keeps the coded refusal")

	for name, body := range map[string]string{"two receipts": `{} {}`, "not JSON": `receipt`} {
		reply(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := client.ImportMerchantBilling(t.Context(), bytes.NewReader(archive))
		require.ErrorIs(t, err, ErrUnreachable, name)
	}

	before := calls.Load()
	other := merchant.WithID(context.Background(), MerchantID(uuid.New()))
	require.ErrorIs(t, client.ExportMerchantBilling(other, io.Discard), ErrConflict)
	_, err = client.ImportMerchantBilling(other, bytes.NewReader(archive))
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, client.ExportMerchantBilling(t.Context(), nil), ErrInvalid)
	_, err = client.ImportMerchantBilling(t.Context(), nil)
	require.ErrorIs(t, err, ErrInvalid)
	require.Equal(t, before, calls.Load(), "refused before reaching the server")
}
