package stripeapi

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/providerposture"
)

type wireFunc func(*http.Request) (*http.Response, error)

func (f wireFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Until its PSP's load has verified a key, a write with it is not sent: it
// waits for that verdict and is refused while it is pending. Reads go out.
func TestAWriteBeforeItsKeysFirstVerdictIsNotSent(t *testing.T) {
	var writes, reads atomic.Int32
	wire := wireFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			reads.Add(1)
		} else {
			writes.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}, Request: r}, nil
	})
	client := &http.Client{Transport: &guardTransport{base: wire}}
	key := "sk_test_guard_" + uuid.NewString()
	send := func(ctx context.Context, method string) error {
		req, err := http.NewRequestWithContext(ctx, method, APIBase+"/v1/customers", strings.NewReader("email=a%40example.com"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+key)
		response, err := client.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}

	pending, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err := send(pending, http.MethodPost)
	require.ErrorIs(t, err, providerposture.ErrDisarmed)
	require.ErrorIs(t, err, providerposture.ErrPending)
	require.NotContains(t, err.Error(), key)
	require.Zero(t, writes.Load(), "a write before the first verdict is not sent")

	require.NoError(t, send(t.Context(), http.MethodGet))
	require.EqualValues(t, 1, reads.Load(), "reads do not wait for posture")

	// A write waiting when the PSP's verification lands goes out on its verdict.
	done := make(chan error, 1)
	go func() { done <- send(t.Context(), http.MethodPost) }()
	time.Sleep(20 * time.Millisecond)
	require.Zero(t, writes.Load())
	status := providerposture.Process().Verify(t.Context(), PostureKey(key, APIBase, ""), func(context.Context) (providerposture.Verdict, error) {
		return providerposture.Simulated, nil
	})
	require.True(t, status.Armed())
	require.NoError(t, <-done)
	require.EqualValues(t, 1, writes.Load())
}
