package billingauth

import (
	"net/http"
)

// ExplicitCredentials is the HTTP authentication boundary of every OpenRails
// mount: ambient cookies never reach an Auth, so a browser call carries its
// credential in a header. Hosts may use it on their own billing-adjacent
// routes to select credentials the same way.
func ExplicitCredentials(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			r = r.Clone(r.Context())
			r.Header.Del("Cookie")
		}
		next.ServeHTTP(w, r)
	})
}
