package controlplane

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestNew_RequiresControlPlaneConfig(t *testing.T) {
	// HARD CUT (#469): the control plane is mandatory — a missing
	// auth.control_plane section is a construction error, never a nil control
	// plane ("verifier-only mode" is gone).
	if _, err := New(context.Background(), &config.Config{}, nil); err == nil {
		t.Fatal("expected error when auth.control_plane is missing")
	}
	if _, err := New(context.Background(), &config.Config{Auth: &config.AuthConfig{}}, nil); err == nil {
		t.Fatal("expected error when auth.control_plane is missing")
	}
}

func TestNew_RequiresPool(t *testing.T) {
	cfg := &config.Config{Auth: &config.AuthConfig{
		ExpectedAudience: "openrails-app",
		ControlPlane: &config.ControlPlaneConfig{
			Issuer: "https://openrails.example.com",
		},
	}}
	if _, err := New(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected error when pool is nil")
	}
}

func TestRegistrationControls_DefaultRestricted(t *testing.T) {
	// Independent registration axes (authkit issue 60). Defaults: both restricted
	// (closed registration); route posture is self-hosted unless BOTH are public.
	cp := &config.ControlPlaneConfig{} // omit both -> restricted
	if cp.UserRegistrationOpen() || cp.TenantRegistrationOpen() {
		t.Error("unset registration flags should default to restricted (false)")
	}
	if !cp.SelfHostedPosture() {
		t.Error("restricted registration should yield self-hosted posture")
	}
	// Only user registration public: still self-hosted (not both).
	cp.PublicUserRegistration = true
	if !cp.UserRegistrationOpen() || cp.TenantRegistrationOpen() {
		t.Error("expected user-reg open, tenant-reg restricted")
	}
	if !cp.SelfHostedPosture() {
		t.Error("one axis public should remain self-hosted posture")
	}
	// Both public: full hosted-SaaS posture.
	cp.PublicTenantRegistration = true
	if cp.SelfHostedPosture() {
		t.Error("both axes public should drop the self-hosted posture")
	}
}
