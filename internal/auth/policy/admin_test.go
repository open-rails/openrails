package policy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// TestIsOperatorAdmin_NoOperatorOrgDeniesGlobalAdmin proves the helper variant of
// the hardcut: with no operator org configured, IsOperatorAdmin returns false
// without consulting the DB (nil db would otherwise panic). There is no
// global-admin DB fallback.
func TestIsOperatorAdmin_NoOperatorOrgDeniesGlobalAdmin(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			ControlPlane: &config.ControlPlaneConfig{Enabled: true},
		},
	}
	got, err := IsOperatorAdmin(context.Background(), cfg, nil, authprovider.UserContext{UserID: "u1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got {
		t.Error("control plane enabled: global-admin fallback must be denied")
	}
}

// TestIsOperatorAdmin_MultiOrg covers the helper that admin handlers like
// catalog.go use to gate inactive-product visibility for read paths. The
// operator-org branch is the only authority path (no DB fallback).
func TestIsOperatorAdmin_MultiOrg(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			OperatorOrgSlug: "acme",
		},
	}

	cases := []struct {
		name string
		uc   authprovider.UserContext
		want bool
	}{
		{name: "no org -> false", uc: authprovider.UserContext{UserID: "u1"}, want: false},
		{name: "wrong org -> false", uc: authprovider.UserContext{UserID: "u1", Org: "globex", OrgRoles: []string{"admin"}}, want: false},
		{name: "right org, wrong role -> false", uc: authprovider.UserContext{UserID: "u1", Org: "acme", OrgRoles: []string{"member"}}, want: false},
		{name: "right org, admin role -> true", uc: authprovider.UserContext{UserID: "u1", Org: "acme", OrgRoles: []string{"admin"}}, want: true},
		{name: "right org, owner role -> true", uc: authprovider.UserContext{UserID: "u1", Org: "acme", OrgRoles: []string{"owner"}}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IsOperatorAdmin(context.Background(), cfg, nil, tc.uc)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAuthConfig_EffectiveOperatorOrgAdminRoles(t *testing.T) {
	// Default fallback when no roles configured.
	cfg := &config.AuthConfig{}
	got := cfg.EffectiveOperatorOrgAdminRoles()
	if len(got) != 2 || got[0] != "admin" || got[1] != "owner" {
		t.Errorf("default roles: got %v want [admin owner]", got)
	}

	// Explicit override.
	cfg = &config.AuthConfig{OperatorOrgAdminRoles: []string{"billing_admin"}}
	got = cfg.EffectiveOperatorOrgAdminRoles()
	if len(got) != 1 || got[0] != "billing_admin" {
		t.Errorf("override: got %v want [billing_admin]", got)
	}
}

func TestAuthConfig_OperatorOrgEnabled(t *testing.T) {
	if (&config.AuthConfig{}).OperatorOrgEnabled() {
		t.Error("empty config: OperatorOrgEnabled should be false")
	}
	if !(&config.AuthConfig{OperatorOrgSlug: "acme"}).OperatorOrgEnabled() {
		t.Error("acme: should be true")
	}
	if (&config.AuthConfig{OperatorOrgSlug: "   "}).OperatorOrgEnabled() {
		t.Error("whitespace-only slug: should be false")
	}
}
