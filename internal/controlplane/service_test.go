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

func TestLockedDownEnabled_DefaultsTrue(t *testing.T) {
	var cp *config.ControlPlaneConfig
	if cp.LockedDownEnabled() {
		t.Error("nil control plane config should not be locked down")
	}
	cp = &config.ControlPlaneConfig{} // LockedDown unset -> default true
	if !cp.LockedDownEnabled() {
		t.Error("unset LockedDown should default to locked-down=true")
	}
	f := false
	cp.LockedDown = &f
	if cp.LockedDownEnabled() {
		t.Error("explicit LockedDown=false should opt out of locked-down mode")
	}
	tr := true
	cp.LockedDown = &tr
	if !cp.LockedDownEnabled() {
		t.Error("explicit LockedDown=true should be locked-down")
	}
}
