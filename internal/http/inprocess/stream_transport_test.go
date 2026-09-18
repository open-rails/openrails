package inprocess

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestStreamInprocessResponseDoesNotBufferTheBook(t *testing.T) {
	book := bytes.Repeat([]byte("archive-data\n"), 200000)
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write(book)
		done <- err
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://openrails.invalid/v1/merchant/billing-archive", nil)
	resp, err := streamInprocessResponse(handler, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-done:
		t.Fatal("handler buffered the entire archive before the caller read it")
	default:
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, book) {
		t.Fatalf("archive stream differs: bytes=%d error=%v", len(got), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestStreamInprocessResponseStopsOnCloseAndCancellation(t *testing.T) {
	t.Run("caller stops reading", func(t *testing.T) {
		done := make(chan error, 1)
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("archive"))
			done <- err
		})
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://openrails.invalid", nil)
		resp, err := streamInprocessResponse(h, req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if err := <-done; !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("producer did not stop: %v", err)
		}
	})
	t.Run("cancel before headers", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { cancel() })
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://openrails.invalid", nil)
		resp, err := streamInprocessResponse(h, req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected cancellation result: %v", err)
		}
	})
	t.Run("panic before headers", func(t *testing.T) {
		h := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("fixture") })
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://openrails.invalid", nil)
		resp, err := streamInprocessResponse(h, req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("panic returned %d", resp.StatusCode)
		}
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Fatal("panic did not interrupt the stream")
		}
	})
}

func TestStreamInprocessCancellationAfterHeaders(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("blocked until cancellation"))
		done <- err
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://openrails.invalid", nil)
	resp, err := streamInprocessResponse(handler, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("blocked producer was not interrupted")
	}
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, context.Canceled) {
		t.Fatalf("body did not report cancellation: %v", err)
	}
}

func TestStreamInprocessCloseCancelsHandler(t *testing.T) {
	done := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		<-req.Context().Done()
		done <- req.Context().Err()
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://openrails.invalid", nil)
	resp, err := streamInprocessResponse(h, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler context: %v", err)
	}
}
