package embedhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/http/router"
)

func TestChainRecoversPanics(t *testing.T) {
	passthrough := func(h http.Handler) http.Handler { return h }
	boom := router.Entry{Method: http.MethodGet, Path: EmbeddedV1Prefix + "/boom", Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })}
	h := (&Assembler{}).chain(boom, passthrough)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, boom.Path, nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.JSONEq(t, `{"error":{"type":"api_error","code":"internal_error","message":"internal server error"}}`, rec.Body.String())
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), "the security headers precede the handler")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, boom.Path, nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code, "the chain serves the next request")

	abort := router.Entry{Method: http.MethodGet, Path: boom.Path, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })}
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		(&Assembler{}).chain(abort, passthrough).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, boom.Path, nil))
	}, "a client-abort panic still propagates to the server")
}
