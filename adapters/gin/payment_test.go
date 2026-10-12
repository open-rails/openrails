package openrailsgin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The payment origin's Host reaches the page alone, however a browser spells
// it; forwarded headers never select it, and a malformed Host is refused.
func TestPaymentOriginRoutesByHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, origin, host, forwarded string
		disabled                      bool
		status                        int
		page                          bool
	}{
		{name: "payment", origin: "https://pay.example", host: "pay.example", status: 202, page: true},
		{name: "case and default port", origin: "https://pay.example", host: "PAY.EXAMPLE:443", status: 202, page: true},
		{name: "DNS trailing dot", origin: "https://pay.example", host: "pay.example.", status: 202, page: true},
		{name: "forwarded cannot escape", origin: "https://pay.example", host: "pay.example", forwarded: "shop.example", status: 202, page: true},
		{name: "app unaffected", origin: "https://pay.example", host: "shop.example", forwarded: "pay.example", status: 204},
		{name: "other port is another origin", origin: "http://127.0.0.1:8081", host: "127.0.0.1:8080", status: 204},
		{name: "loopback port", origin: "http://127.0.0.1:8081", host: "127.0.0.1:8081", status: 202, page: true},
		{name: "empty port", origin: "https://pay.example", host: "pay.example:", status: 400},
		{name: "bad host", origin: "https://pay.example", host: "pay.example/path", status: 400},
		{name: "no page here", origin: "https://pay.example", host: "pay.example", disabled: true, status: 404},
		{name: "no payment origin", origin: "", host: "pay.example", status: 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := false
			var page http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { served = true; w.WriteHeader(202) })
			if tc.disabled {
				page = nil
			}
			pay, err := PaymentOrigin(tc.origin, page)
			require.NoError(t, err)
			r := gin.New()
			r.Use(pay)
			r.GET("/v1/me/orders/:id", func(c *gin.Context) { c.Status(204) })
			r.NoRoute(func(c *gin.Context) { c.Status(204) })
			for _, path := range []string{"/pay/ord_1", "/v1/me/orders/ord_1", "/admin"} {
				served = false
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Host = tc.host
				req.Header.Set("X-Forwarded-Host", tc.forwarded)
				out := httptest.NewRecorder()
				r.ServeHTTP(out, req)
				require.Equal(t, tc.status, out.Code, path)
				require.Equal(t, tc.page, served, path)
			}
		})
	}
	_, err := PaymentOrigin("https://pay.example/path", nil)
	require.Error(t, err)
}

// A payment origin request logs without its query string or error details;
// so does a /pay/ path wherever it arrived.
func TestRedactLogOmitsPaymentDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	pay, err := PaymentOrigin("https://pay.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	require.NoError(t, err)
	var logged []gin.LogFormatterParams
	var marked []bool
	r := gin.New()
	r.Use(gin.LoggerWithConfig(gin.LoggerConfig{Output: ioDiscard{}, Formatter: RedactLog(func(p gin.LogFormatterParams) string {
		logged = append(logged, p)
		return ""
	})}))
	r.Use(func(c *gin.Context) { c.Next(); marked = append(marked, PaymentRequest(c)) })
	r.Use(pay)
	r.GET("/shop", func(c *gin.Context) { _ = c.Error(errSecret); c.Status(500) })
	r.GET("/pay/:id", func(c *gin.Context) { _ = c.Error(errSecret); c.Status(500) })
	for _, tc := range []struct{ host, path string }{
		{"pay.example", "/v1/me/orders/ord_1?client_secret=x"},
		{"shop.example", "/pay/ord_1?x=y"},
		{"shop.example", "/shop?q=1"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Host = tc.host
		r.ServeHTTP(httptest.NewRecorder(), req)
	}
	require.Len(t, logged, 3)
	require.Equal(t, "/v1/me/orders/ord_1", logged[0].Path)
	require.Equal(t, "/pay/ord_1", logged[1].Path)
	require.Equal(t, "payment origin error details omitted", logged[1].ErrorMessage)
	require.Equal(t, "/shop?q=1", logged[2].Path)
	require.Contains(t, logged[2].ErrorMessage, errSecret.Error())
	require.Equal(t, []bool{true, true, false}, marked)
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

var errSecret = errors.New("pus_secret detail")
