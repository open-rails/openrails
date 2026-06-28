package controlplane

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestNew_RequiresAuthIssuer(t *testing.T) {
	// HARD CUT (#469): the control plane is mandatory; standalone needs an
	// issuer and there is no verifier-only mode.
	if _, err := New(context.Background(), &config.Config{}, nil); err == nil {
		t.Fatal("expected error when auth is missing")
	}
	if _, err := New(context.Background(), &config.Config{Auth: &config.AuthConfig{}}, nil); err == nil {
		t.Fatal("expected error when auth.issuer is missing")
	}
}

func TestNew_RequiresPool(t *testing.T) {
	cfg := &config.Config{Auth: &config.AuthConfig{Issuer: "https://openrails.example.com"}}
	if _, err := New(context.Background(), cfg, nil); err == nil {
		t.Fatal("expected error when pool is nil")
	}
}
