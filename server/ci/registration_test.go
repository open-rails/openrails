//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/server"
)

// Local staff enrollment defaults to invitation-only. Explicit closed stays
// restrictive, standalone open is refused, and hosted open needs its creation
// policy and verified contact. Issuer-only servers expose no enrollment.
func TestRegistrationModeReachesAuthKit(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		mode     iam.RegistrationMode
		register int
		hosted   bool
	}{
		{"", http.StatusForbidden, false},
		{iam.RegistrationModeClosed, http.StatusNotFound, false},
		{iam.RegistrationModeInviteOnly, http.StatusForbidden, false},
		{iam.RegistrationModeOpen, http.StatusAccepted, true},
	} {
		name := string(tc.mode)
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			issuer := "http://127.0.0.1/reg-" + uuid.NewString()[:8]
			srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
				cfg.Auth.Token.Issuer = issuer
				cfg.Auth.Registration.NativeUserMode = tc.mode
				if tc.hosted {
					cfg.MerchantCreation = &server.MerchantCreationConfig{}
				}
				if tc.mode != iam.RegistrationModeClosed {
					deps.Engine.Email = &outbox{}
				}
			})
			handler, err := standaloneHandler(srv)
			require.NoError(t, err)
			base := issuer
			base = base[strings.Index(base, "/reg-"):] + "/v1"

			w := get(handler, base+"/capabilities")
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var caps struct {
				Registration struct {
					Mode string `json:"mode"`
				} `json:"registration"`
			}
			require.NoError(t, json.NewDecoder(w.Body).Decode(&caps))
			want := tc.mode
			if want == "" {
				want = iam.RegistrationModeInviteOnly
			}
			require.Equal(t, string(want), caps.Registration.Mode)

			id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
			body, err := json.Marshal(map[string]string{"identifier": "reg-" + id + "@e2e.test", "username": "reg_" + id, "password": "correct-horse-battery-" + id})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, base+"/register", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			require.Equal(t, tc.register, rec.Code, rec.Body.String())
		})
	}

	t.Run("refuses", func(t *testing.T) {
		for mode, want := range map[iam.RegistrationMode]string{
			"sometimes":                    "not one of open, invite_only, closed",
			iam.RegistrationModeOpen:       "open self-registration",
			iam.RegistrationModeInviteOnly: "no email or SMS sender is configured",
		} {
			_, err := f.buildServer(t, func(cfg *server.Config, _ *server.Deps) {
				cfg.Auth.Token.Issuer = "http://127.0.0.1/reg-" + uuid.NewString()[:8]
				cfg.Auth.Registration.AllowMissingSenders = false
				cfg.Auth.Registration.Verification = iam.RegistrationVerificationRequired
				cfg.Auth.Registration.NativeUserMode = mode
			})
			require.ErrorContains(t, err, want, mode)
		}
	})

	t.Run("hosted open requires verification sender", func(t *testing.T) {
		_, err := f.buildServer(t, func(cfg *server.Config, _ *server.Deps) {
			cfg.MerchantCreation = &server.MerchantCreationConfig{}
			cfg.Auth.Registration.NativeUserMode = iam.RegistrationModeOpen
			cfg.Auth.Registration.AllowMissingSenders = false
			cfg.Auth.Registration.Verification = iam.RegistrationVerificationRequired
		})
		require.ErrorContains(t, err, "no email or SMS sender is configured")
	})
}

// outbox is an engine email sender that keeps what it sends.
type outbox struct {
	mu   sync.Mutex
	sent []openrails.Email
}

func (o *outbox) Send(_ context.Context, m openrails.Email) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sent = append(o.sent, m)
	return nil
}

func (*outbox) CheckHealth(context.Context) error { return nil }
