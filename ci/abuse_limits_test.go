//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// staffMember admits every request as one member of the merchant's staff.
type staffMember struct{}

const staffMemberID = "7f6c1f0e-4a7b-4f5e-9a51-2a8f0c3b9d11"

func (staffMember) Required() func(http.Handler) http.Handler                { return pass }
func (staffMember) RequirePermission(string) func(http.Handler) http.Handler { return pass }
func (staffMember) Sensitive() func(http.Handler) http.Handler               { return pass }
func (staffMember) Identity(context.Context) (openrails.Identity, bool) {
	return openrails.Identity{Issuer: "test", Subject: staffMemberID, SubjectKind: openrails.SubjectUser,
		Invoker: openrails.Invoker{Issuer: "test", ID: staffMemberID}, Credential: openrails.Credential{Kind: openrails.CredentialSession, ID: "s_staff"}}, true
}

// Three replicas without Redis count one set of abuse limits: the per-address
// rate limit, an admin's lockout and a captcha challenge set on one replica
// hold on the others, as they would through Redis.
func TestReplicasShareAbuseLimitsWithoutRedis(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: "limits-" + uuid.NewString()[:8], DisplayName: "Limits"}
	cfg.Captcha = &openrails.CaptchaConfig{SiteKey: "e2e-site", SecretKey: "e2e-secret"}
	// Each replica serves buyers, and its staff on another mount.
	public, staff := make([]http.Handler, 3), make([]http.Handler, 3)
	for i := range public {
		client, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
		buyers, admins := http.NewServeMux(), http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(buyers, client, openrails.Routes{Auth: authtest.Deny{}}))
		require.NoError(t, openrailshttp.Mount(admins, client, openrails.Routes{Auth: staffMember{}, Permissions: staffPermissions}))
		public[i], staff[i] = buyers, admins
	}
	call := func(on []http.Handler, replica int, method, path, addr string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.RemoteAddr = addr
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		on[replica%len(on)].ServeHTTP(rec, req)
		return rec
	}
	pay := func(replica int, addr string) *httptest.ResponseRecorder {
		return call(public, replica, http.MethodPost, "/v1/checkout-sessions/ocs_"+strings.ReplaceAll(uuid.NewString(), "-", "")+"/pay", addr)
	}

	t.Run("rate limit", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		const addr = "198.51.100.7:4711"
		for i := range 10 { // the checkout bucket's 10 a minute
			require.NotEqual(t, http.StatusTooManyRequests, pay(i, addr).Code, "request %d", i+1)
		}
		for i := range public {
			rec := pay(i, addr)
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "replica %d counts the same window: %s", i, rec.Body.String())
		}
	})

	t.Run("admin lockout", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		remove := func(replica int) *httptest.ResponseRecorder {
			return call(staff, replica, http.MethodDelete, "/v1/admin/catalog/rate-overrides/"+uuid.NewString()+"/meter", "198.51.100.8:4711")
		}
		for i := range 5 { // five destructive operations a minute
			require.NotEqual(t, http.StatusTooManyRequests, remove(i).Code, "operation %d", i+1)
		}
		require.Equal(t, http.StatusTooManyRequests, remove(5).Code, "the sixth locks the admin out")
		for i := range staff {
			rec := remove(i)
			require.Equal(t, http.StatusTooManyRequests, rec.Code, "replica %d holds the lockout: %s", i, rec.Body.String())
			retry, err := strconv.Atoi(rec.Header().Get("Retry-After"))
			require.NoError(t, err)
			require.Greater(t, retry, 60, "for the lockout's hour")
		}
	})

	t.Run("captcha challenge", func(t *testing.T) {
		oneMinute(t, 10*time.Second)
		const addr = "198.51.100.9:4711"
		var last *httptest.ResponseRecorder
		for range 30 { // three times the limit challenges the address
			last = pay(0, addr)
		}
		require.Equal(t, "true", last.Header().Get("X-Captcha-Required"), last.Body.String())
		for i := 1; i < len(public); i++ {
			rec := call(public, i, http.MethodGet, "/v1/me/payment-methods", addr)
			require.Equal(t, "true", rec.Header().Get("X-Captcha-Required"), "replica %d challenges the address too: %s", i, rec.Body.String())
			require.Contains(t, rec.Body.String(), "captcha_required")
		}
	})
}
