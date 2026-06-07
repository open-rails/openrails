package policy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/authprovider"
)

// TestIsOperatorAdmin_NoControlPlaneDeniesGlobalAdmin proves the helper variant of
// the hardcut: without control-plane operator authority, IsOperatorAdmin returns
// false without consulting the DB (nil db would otherwise panic). There is no
// global-admin DB fallback.
func TestIsOperatorAdmin_NoOperatorTenantDeniesGlobalAdmin(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{},
	}
	got, err := IsOperatorAdmin(context.Background(), cfg, nil, authprovider.UserContext{UserID: "u1"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got {
		t.Error("control plane enabled: global-admin fallback must be denied")
	}
}

// TestIsOperatorAdmin_ControlPlaneDefaultTenant covers the helper that admin handlers like
// catalog.go use to gate inactive-product visibility for read paths. The
// operator tenant branch is the only authority path (no DB fallback).
func TestIsOperatorAdmin_MultiOrg(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			ControlPlane: &config.ControlPlaneConfig{Enabled: true},
		},
	}

	cases := []struct {
		name string
		uc   authprovider.UserContext
		want bool
	}{
		{name: "no org -> false", uc: authprovider.UserContext{UserID: "u1"}, want: false},
		{name: "wrong tenant -> false", uc: authprovider.UserContext{UserID: "u1", Tenant: "globex", TenantRoles: []string{"admin"}}, want: false},
		{name: "right tenant, wrong role -> false", uc: authprovider.UserContext{UserID: "u1", Tenant: "operator", TenantRoles: []string{"member"}}, want: false},
		{name: "right tenant, admin role -> true", uc: authprovider.UserContext{UserID: "u1", Tenant: "operator", TenantRoles: []string{"admin"}}, want: true},
		{name: "right tenant, owner role -> true", uc: authprovider.UserContext{UserID: "u1", Tenant: "operator", TenantRoles: []string{"owner"}}, want: true},
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

func TestAuthConfig_EffectiveOperatorTenantAdminRoles(t *testing.T) {
	// Default fallback when no roles configured.
	cfg := &config.AuthConfig{}
	got := cfg.EffectiveOperatorTenantAdminRoles()
	if len(got) != 2 || got[0] != "admin" || got[1] != "owner" {
		t.Errorf("default roles: got %v want [admin owner]", got)
	}

	// Explicit override.
	cfg = &config.AuthConfig{OperatorTenantAdminRoles: []string{"billing_admin"}}
	got = cfg.EffectiveOperatorTenantAdminRoles()
	if len(got) != 1 || got[0] != "billing_admin" {
		t.Errorf("override: got %v want [billing_admin]", got)
	}
}

func TestAuthConfig_OperatorTenantEnabled(t *testing.T) {
	if (&config.AuthConfig{}).OperatorTenantEnabled() {
		t.Error("empty config: OperatorTenantEnabled should be false")
	}
	if !(&config.AuthConfig{OperatorTenantSlug: "acme"}).OperatorTenantEnabled() {
		t.Error("acme: should be true")
	}
	if (&config.AuthConfig{OperatorTenantSlug: "   "}).OperatorTenantEnabled() {
		t.Error("whitespace-only slug: should be false")
	}
	if !(&config.AuthConfig{ControlPlane: &config.ControlPlaneConfig{Enabled: true}}).OperatorTenantEnabled() {
		t.Error("control plane enabled: should be true")
	}
}
