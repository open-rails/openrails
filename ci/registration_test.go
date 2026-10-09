//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// Registration is AuthKit's mode, passed straight through: capabilities
// report it, closed mounts no register route, invite-only needs an invite
// code, open registers a verified contact. A mode outside AuthKit's three, or
// registration without a sender, refuses to boot.
func TestRegistrationModeReachesAuthKit(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool, f.schema))
	for _, tc := range []struct {
		mode     iam.RegistrationMode
		register int
	}{
		{"", http.StatusNotFound},
		{iam.RegistrationModeClosed, http.StatusNotFound},
		{iam.RegistrationModeInviteOnly, http.StatusForbidden},
		{iam.RegistrationModeOpen, http.StatusAccepted},
	} {
		name := string(tc.mode)
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			cfg := f.config()
			cfg.ControlPlane = controlPlane(t, "http://127.0.0.1/reg-"+uuid.NewString()[:8])
			cfg.ControlPlane.Registration = tc.mode
			deps := openrails.Deps{Postgres: f.pool}
			if tc.mode == iam.RegistrationModeOpen || tc.mode == iam.RegistrationModeInviteOnly {
				deps.Email = &outbox{}
			}
			client, err := openrails.New(t.Context(), cfg, deps)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close(context.Background()) })
			handler, err := standaloneHandler(client)
			require.NoError(t, err)
			base := strings.TrimSuffix(cfg.ControlPlane.Auth.Issuer, "/")
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
				want = iam.RegistrationModeClosed
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
			iam.RegistrationModeOpen:       "requires an email or SMS sender",
			iam.RegistrationModeInviteOnly: "requires an email or SMS sender",
		} {
			cfg := f.config()
			cfg.ControlPlane = controlPlane(t, "http://127.0.0.1/reg-"+uuid.NewString()[:8])
			cfg.ControlPlane.Auth.AllowMissingSenders = false
			cfg.ControlPlane.Registration = mode
			client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
			if err == nil {
				_ = client.Close(context.Background())
			}
			require.ErrorContains(t, err, want, mode)
		}
	})
}
