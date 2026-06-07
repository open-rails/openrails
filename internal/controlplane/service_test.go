package controlplane

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestNew_DisabledReturnsNil(t *testing.T) {
	// No auth config at all.
	cp, err := New(context.Background(), &config.Config{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp != nil {
		t.Fatal("expected nil control plane when disabled")
	}

	// Auth present but control plane disabled.
	cfg := &config.Config{Auth: &config.AuthConfig{}}
	cp, err = New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cp != nil {
		t.Fatal("expected nil control plane when control plane disabled")
	}
}

func TestNew_EnabledRequiresPool(t *testing.T) {
	cfg := &config.Config{Auth: &config.AuthConfig{
		ExpectedAudience: "openrails-app",
		ControlPlane: &config.ControlPlaneConfig{
			Enabled: true,
			Issuer:  "https://billing.example.com",
		},
	}}
	if _, err := New(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected error when pool is nil and control plane enabled")
	}
}

func TestControlPlaneEnabled(t *testing.T) {
	var a *config.AuthConfig
	if a.ControlPlaneEnabled() {
		t.Error("nil auth config should not enable control plane")
	}
	a = &config.AuthConfig{}
	if a.ControlPlaneEnabled() {
		t.Error("empty auth config should not enable control plane")
	}
	a.ControlPlane = &config.ControlPlaneConfig{Enabled: false}
	if a.ControlPlaneEnabled() {
		t.Error("disabled control plane should report disabled")
	}
	a.ControlPlane.Enabled = true
	if !a.ControlPlaneEnabled() {
		t.Error("enabled control plane should report enabled")
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
