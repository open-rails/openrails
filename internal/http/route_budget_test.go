package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
)

// xs-007 row 37: a route that declares its budget owns its deadline. The
// server (standalone or a host's) may carry a WriteTimeout set before the
// handler knew its work; the route lifts it through the middleware writers,
// so a response to work that already committed is never cut off by a clock
// the route did not declare.
func TestRouteBudget_OutlivesServerWriteTimeout(t *testing.T) {
	const serverWrite = 300 * time.Millisecond
	const work = 3 * serverWrite

	handler := func(budgeted bool) http.Handler {
		return middleware.RequestLogHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := httprequest.NewHTTP(w, r, nil)
			if budgeted {
				_, cancel := req.Budget(10 * work)
				defer cancel()
			}
			time.Sleep(work) // the provider round-trips the budget was derived from
			req.SuccessJSON(map[string]any{"committed": true})
		}))
	}

	serve := func(h http.Handler) (status int, body string, err error) {
		ln, lerr := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, lerr)
		srv := &http.Server{Handler: h, WriteTimeout: serverWrite, ReadHeaderTimeout: time.Second}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		resp, rerr := http.Get("http://" + ln.Addr().String() + "/")
		if rerr != nil {
			return 0, "", rerr
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	// Negative control: without a declared budget the server's clock wins and
	// the client sees a dropped connection after the work committed.
	_, _, err := serve(handler(false))
	require.Error(t, err, "the server-wide write deadline cuts the response off")

	status, body, err := serve(handler(true))
	require.NoError(t, err, "the route's budget lifted the connection deadline")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, `"committed":true`)

	// Sanity: the unwrapped writer path also works for a bare httptest recorder
	// (no connection to set a deadline on — Budget must not fail the request).
	rec := httptest.NewRecorder()
	req := httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil), nil)
	_, cancel := req.Budget(time.Second)
	cancel()
	req.SuccessJSON(map[string]any{"ok": true})
	require.Equal(t, http.StatusOK, rec.Code)
}
