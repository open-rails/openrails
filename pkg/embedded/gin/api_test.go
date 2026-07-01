package gin

import (
	"net/http"
	"testing"
)

func TestBillingMethodsExcludeConnectAndTrace(t *testing.T) {
	seen := map[string]bool{}
	for _, method := range billingMethods {
		seen[method] = true
	}
	for _, method := range []string{
		http.MethodGet,
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodHead,
	} {
		if !seen[method] {
			t.Fatalf("billingMethods missing %s", method)
		}
	}
	if seen[http.MethodConnect] || seen[http.MethodTrace] {
		t.Fatalf("billingMethods must not register CONNECT or TRACE: %v", billingMethods)
	}
}
