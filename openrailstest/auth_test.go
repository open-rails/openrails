package openrailstest_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/openrailstest"
)

func request(token string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/payments", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}
}

// recorder captures CheckAuth's failures instead of failing the test.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Errorf(format string, args ...any) { r.failures = append(r.failures, format) }
func (r *recorder) Fatal(args ...any)                 { r.failures = append(r.failures, "fatal") }
func (r *recorder) Helper()                           {}

func TestCheckAuthPassesAConformingAuth(t *testing.T) {
	fake := &authtest.Fake{}
	staff := fake.Person("22222222-2222-4222-8222-222222222222", billing.MerchantPaymentsRefund)
	stale := fake.Issue(authtest.Grant{Identity: authtest.User("33333333-3333-4333-8333-333333333333"), Permissions: []string{billing.MerchantPaymentsRefund}, Stale: true})
	openrailstest.CheckAuth(t, fake, openrailstest.AuthCases{
		Permission: billing.MerchantPaymentsRefund,
		Customer:   request(fake.Person("11111111-1111-4111-8111-111111111111")),
		Staff:      request(staff),
		Refused:    map[string]func() *http.Request{"forged": request("test_forged"), "malformed": request("not-a-token")},
		StaleStaff: request(stale),
		Machine:    request(fake.Machine("key_1", billing.MerchantPaymentsRefund)),
	})
}

func TestCheckAuthCatchesAPassThroughAuth(t *testing.T) {
	r := &recorder{TB: t}
	openrailstest.CheckAuth(r, authtest.PassThrough{}, openrailstest.AuthCases{
		Permission: billing.MerchantPaymentsRefund,
		Customer:   request("anyone"),
		Staff:      request("anyone"),
		Refused:    map[string]func() *http.Request{"forged": request("forged")},
	})
	if len(r.failures) < 4 {
		t.Fatalf("a pass-through Auth drew %d failures: %v", len(r.failures), r.failures)
	}
}
