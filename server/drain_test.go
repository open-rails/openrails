package server

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// listen serves h on a loopback port.
func listen(t *testing.T, h http.Handler) (*http.Server, string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return hs, "http://" + ln.Addr().String(), served
}

type reply struct {
	status int
	body   string
	close  bool
}

// get is one request on its own connection.
func get(url string) (reply, error) {
	client := &http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return reply{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, string(body), resp.Close}, err
}

// A stop marks the process unready at once, keeps serving through the drain,
// then closes the listener and still answers the request already in flight.
func TestDrainServesThenFinishesInFlight(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	hs, url, served := listen(t, mux)

	slow := make(chan reply, 1)
	go func() {
		r, err := get(url + "/slow")
		if err != nil {
			t.Error(err)
		}
		slow <- r
	}()
	<-started

	const delay = 400 * time.Millisecond
	unready := make(chan struct{})
	stopped := make(chan error, 1)
	begin := time.Now()
	go func() {
		err := drain(func() { close(unready) }, delay, served, hs)
		shutdown(time.Now().Add(10*time.Second), hs)
		stopped <- err
	}()
	<-unready

	ok, err := get(url + "/ok")
	require.NoError(t, err, "the drain keeps serving")
	require.Equal(t, http.StatusNoContent, ok.status)
	require.True(t, ok.close, "with keep-alives off")

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", url[len("http://"):], 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
		}
		return err != nil
	}, 5*time.Second, 20*time.Millisecond, "the listener closes after the drain")
	require.GreaterOrEqual(t, time.Since(begin), delay)
	select {
	case <-slow:
		t.Fatal("the in-flight request ended before it was released")
	case <-stopped:
		t.Fatal("shutdown returned with a request in flight")
	default:
	}

	close(release)
	require.Equal(t, reply{http.StatusOK, "done", true}, <-slow)
	require.NoError(t, <-stopped)
}

// A request that never finishes holds the stop only until the deadline.
func TestShutdownDeadlineClosesStuckRequests(t *testing.T) {
	started, never := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(never) })
	hs, url, _ := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-never:
		case <-r.Context().Done():
		}
	}))
	failed := make(chan error, 1)
	go func() {
		_, err := get(url)
		failed <- err
	}()
	<-started

	begin := time.Now()
	shutdown(time.Now().Add(300*time.Millisecond), hs)
	require.Less(t, time.Since(begin), 3*time.Second)
	require.Error(t, <-failed, "the stuck connection is closed")
}

// Without a drain the stop starts at once and readiness is left alone.
func TestNoDrainDelay(t *testing.T) {
	hs, _, served := listen(t, http.NotFoundHandler())
	require.NoError(t, drain(func() { t.Fatal("unready without a drain") }, 0, served, hs))
}
