package openrailsgin

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/open-rails/openrails/internal/paymentpage"
)

type paymentKey struct{}

// PaymentOrigin serves the payment origin ahead of every other route: a
// request whose Host is origin reaches only page (Engine.PaymentPage's
// handler), so no app, sign-in or admin route answers there; a malformed Host
// is refused with 400; any other request passes on. Forwarded headers never
// select it. Register it before other middleware and routes:
//
//	pay, err := openrailsgin.PaymentOrigin(origin, page)
//	router.Use(pay)
//
// A nil page answers 404 on the payment origin (another host serves it), and
// "" serves none. Its requests are marked for RedactLog and PaymentRequest.
func PaymentOrigin(origin string, page http.Handler) (gin.HandlerFunc, error) {
	if origin == "" {
		return func(c *gin.Context) { c.Next() }, nil
	}
	host, err := paymentpage.NewHost(origin)
	if err != nil {
		return nil, err
	}
	return func(c *gin.Context) {
		match, valid := host.Match(c.Request.Host)
		switch {
		case !valid:
			c.AbortWithStatus(http.StatusBadRequest)
		case !match:
			c.Next()
		case page == nil:
			c.Set(paymentKey{}, true)
			c.AbortWithStatus(http.StatusNotFound)
		default:
			c.Set(paymentKey{}, true)
			// An unrouted path reaches here through NoRoute, which presets 404;
			// the page sets its own status only for errors.
			c.Status(http.StatusOK)
			page.ServeHTTP(c.Writer, c.Request)
			c.Abort()
		}
	}, nil
}

// PaymentRequest reports whether c is a payment origin request, or a payment
// page path (/pay/...) wherever it arrived: logs and panic recovery leave out
// its query string and error details.
func PaymentRequest(c *gin.Context) bool {
	return c != nil && (paymentMarked(c.Keys) || paymentPath(c.Request.URL.Path))
}

// RedactLog wraps a gin log formatter (gin.LoggerWithFormatter): a payment
// origin request is logged without its query string or error details.
func RedactLog(format gin.LogFormatter) gin.LogFormatter {
	return func(p gin.LogFormatterParams) string {
		if paymentMarked(p.Keys) || paymentPath(p.Path) {
			p.Path, _, _ = strings.Cut(p.Path, "?")
			if p.ErrorMessage != "" {
				p.ErrorMessage = "payment origin error details omitted"
			}
		}
		return format(p)
	}
}

func paymentMarked(keys map[any]any) bool {
	marked, _ := keys[paymentKey{}].(bool)
	return marked
}

func paymentPath(path string) bool { return strings.HasPrefix(path, paymentpage.PagePrefix) }
