package openrails

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientDeadlineOwnership(t *testing.T) {
	for _, mode := range []string{"default unbounded", "caller deadline", "caller cancellation", "explicit timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cancel := func() {}
			deadline := time.Now().Add(time.Minute)
			if mode == "caller cancellation" {
				ctx, cancel = context.WithCancel(ctx)
			} else if mode != "default unbounded" {
				ctx, cancel = context.WithDeadline(ctx, deadline)
			}
			defer cancel()
			called := false
			transport := deadlineTransport(func(request *http.Request) (*http.Response, error) {
				called = true
				observed, hasDeadline := request.Context().Deadline()
				switch mode {
				case "default unbounded":
					if hasDeadline {
						t.Errorf("default introduced a deadline: %v", observed)
					}
				case "caller deadline":
					if !hasDeadline || !observed.Equal(deadline) {
						t.Errorf("caller deadline changed: got %v, want %v", observed, deadline)
					}
				case "caller cancellation":
					cancel()
					<-request.Context().Done()
					return nil, request.Context().Err()
				case "explicit timeout":
					if !hasDeadline || !observed.Before(deadline) {
						t.Errorf("explicit timeout did not shorten caller budget: %v", observed)
					}
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
			})
			opts := []ClientOption{WithAPIKey("test-key"), WithDefaultMerchant("fixture"), WithHTTPClient(&http.Client{Transport: transport})}
			if mode == "explicit timeout" {
				opts = append(opts, WithTimeout(20*time.Millisecond))
			}
			client, err := NewRemote("https://openrails.test", opts...)
			if err != nil {
				t.Fatal(err)
			}
			err = client.Verify(ctx)
			switch mode {
			case "caller cancellation":
				if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrUnreachable) {
					t.Fatalf("cancellation not preserved: %v", err)
				}
			case "explicit timeout":
				if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrUnreachable) {
					t.Fatalf("explicit deadline not enforced: %v", err)
				}
				if ctx.Err() != nil {
					t.Fatal("child timeout canceled caller context")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if !called {
				t.Fatal("test did not exercise the HTTP boundary")
			}
		})
	}
}
