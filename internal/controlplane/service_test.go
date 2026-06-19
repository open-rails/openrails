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
		ControlPlane: &config.ControlPlaneConfig{
			Issuer: "https://openrails.example.com",
		},
	}}
	if _, err := New(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected error when pool is nil")
	}
}

func TestRegistrationControls_DefaultRestricted(t *testing.T) {
	// Default posture is private/self-hosted: no public native-user or org
	// registration, and only the intentional AuthKit route groups are mounted.
	cp := &config.ControlPlaneConfig{}
	if !cp.SelfHostedPosture() {
		t.Error("restricted registration should yield self-hosted posture")
	}
	cp.PublicHosted = true
	if cp.SelfHostedPosture() {
		t.Error("public hosted mode should drop the self-hosted posture")
	}
}
