package openrails

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	archiveformat "github.com/open-rails/openrails/internal/merchantarchive/format"
	"github.com/open-rails/openrails/pkg/merchant"
)

func archiveTransportFixture(t *testing.T, mid MerchantID, customers int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := archiveformat.NewWriter(&buf, mid.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range archiveformat.Profiles {
		if err := w.Table(p); err != nil {
			t.Fatal(err)
		}
		if p.Name != "customers" {
			continue
		}
		for i := 0; i < customers; i++ {
			values := []*string{}
			for _, c := range p.Columns {
				value := ""
				switch c.Name {
				case "merchant_id":
					value = mid.String()
				case "id":
					value = uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprint(i))).String()
				case "issuer":
					value = "https://merchant.example/auth"
				case "created_at", "last_seen_at":
					value = "2026-09-17 12:00:00+00"
				default:
					t.Fatalf("unclassified customer fixture column %s", c.Name)
				}
				values = append(values, &value)
			}
			if err := w.Row(values); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A realistic book is larger than the ordinary 1 MiB JSON envelope. Exercise
// both actual HTTP directions while retaining credentials and merchant binding.
func TestMerchantArchiveLargeHTTPRoundTrip(t *testing.T) {
	mid := MerchantID(uuid.New())
	archive := archiveTransportFixture(t, mid, 7000)
	if len(archive) <= 1<<20 {
		t.Fatal("fixture must exceed the ordinary JSON response/body cap")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != merchantBillingArchivePath || r.Header.Get("Authorization") != "Bearer archive-owner" || r.Header.Get(merchant.BindingHeader) != mid.String() {
			t.Error("archive transport lost its route, credential or merchant assertion")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(archive)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Error("unexpected archive import method/content type")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, archive) {
			t.Error("uploaded archive changed or was truncated")
		}
		_, _ = fmt.Fprintf(w, `{"merchant_id":%q,"digest":"receipt","rows":"7000","already_imported":false}`, mid.String())
	}))
	defer server.Close()
	c, err := NewRemote(server.URL, WithAPIKey("archive-owner"), WithMerchantID(mid))
	if err != nil {
		t.Fatal(err)
	}
	var dst bytes.Buffer
	if err := c.ExportMerchantBilling(context.Background(), &dst); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archive, dst.Bytes()) {
		t.Fatal("downloaded archive differs")
	}
	result, err := c.ImportMerchantBilling(context.Background(), bytes.NewReader(dst.Bytes()))
	if err != nil || result == nil || result.MerchantID != mid || result.Rows != 7000 {
		t.Fatalf("receipt: %+v, %v", result, err)
	}
}

func TestMerchantArchiveRefusalTruncationAndBinding(t *testing.T) {
	mid := MerchantID(uuid.New())
	archive := archiveTransportFixture(t, mid, 1)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodPost {
			w.Header().Set("X-Request-ID", "archive-request")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"billing_archive_not_empty","message":"destination billing state must be empty"}}`))
			return
		}
		_, _ = w.Write(archive[:len(archive)-8])
	}))
	defer server.Close()
	c, err := NewRemote(server.URL, WithAPIKey("owner"), WithMerchantID(mid))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ExportMerchantBilling(context.Background(), io.Discard); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("truncated success must refuse: %v", err)
	}
	_, err = c.ImportMerchantBilling(context.Background(), bytes.NewReader(archive))
	var status *StatusError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &status) || status.Code != "billing_archive_not_empty" || status.RequestID != "archive-request" {
		t.Fatalf("lost coded refusal: %v", err)
	}
	before := calls
	ctx := merchant.WithID(context.Background(), MerchantID(uuid.New()))
	if err := c.ExportMerchantBilling(ctx, io.Discard); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting merchant export: %v", err)
	}
	if _, err := c.ImportMerchantBilling(ctx, bytes.NewReader(archive)); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting merchant import: %v", err)
	}
	if calls != before {
		t.Fatal("conflicting merchant reached the server")
	}
}
