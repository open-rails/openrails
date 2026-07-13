package billingauth

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// TestHasPermission locks the wildcard-grant contract both hosts relied on.
func TestHasPermission(t *testing.T) {
	tests := []struct {
		name       string
		grants     []string
		permission string
		want       bool
	}{
		{"exact match", []string{"billing:read"}, "billing:read", true},
		{"global star", []string{"*"}, "anything:at:all", true},
		{"prefix wildcard covers child", []string{"billing:*"}, "billing:read", true},
		{"prefix wildcard covers deep child", []string{"billing:*"}, "billing:admin:refund", true},
		{"prefix wildcard covers the bare prefix", []string{"billing:*"}, "billing:", true},
		{"wildcard does not cover sibling", []string{"billing:*"}, "catalog:read", false},
		{"whitespace trimmed", []string{"  billing:read  "}, "billing:read", true},
		{"empty grants", nil, "billing:read", false},
		{"no match", []string{"catalog:read", "media:write"}, "billing:read", false},
		{"star must be whole grant", []string{"bill*"}, "billing:read", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasPermission(tt.grants, tt.permission); got != tt.want {
				t.Fatalf("HasPermission(%v, %q) = %v, want %v", tt.grants, tt.permission, got, tt.want)
			}
		})
	}
}

// TestDelegatedGateAuthorize locks the gate's status mapping: nil authn -> 500,
// authn failure -> 401, missing permission -> 403, bad merchant id -> 401,
// success -> mapped Principal.
func TestDelegatedGateAuthorize(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/billing/v1/me", nil)
	base := &DelegatedPrincipal{
		MerchantID:   "00000000-0000-0000-0000-000000000001",
		MerchantSlug: "doujins",
		SubjectID:    "user-1",
		Permissions:  []string{"billing:*"},
		Email:        "u@example.com",
		Username:     "u",
	}

	t.Run("nil authenticator -> 500", func(t *testing.T) {
		_, err := NewDelegatedGate(nil).Authorize(context.Background(), req, "billing:read")
		var ge GateError
		if !errors.As(err, &ge) || ge.Status != http.StatusInternalServerError {
			t.Fatalf("err = %v, want 500 GateError", err)
		}
	})

	t.Run("authn failure -> 401", func(t *testing.T) {
		g := NewDelegatedGate(DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*DelegatedPrincipal, error) {
			return nil, errors.New("nope")
		}))
		_, err := g.Authorize(context.Background(), req, "billing:read")
		var ge GateError
		if !errors.As(err, &ge) || ge.Status != http.StatusUnauthorized {
			t.Fatalf("err = %v, want 401 GateError", err)
		}
	})

	t.Run("missing permission -> 403", func(t *testing.T) {
		p := *base
		p.Permissions = []string{"catalog:read"}
		g := NewDelegatedGate(DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*DelegatedPrincipal, error) {
			return &p, nil
		}))
		_, err := g.Authorize(context.Background(), req, "billing:read")
		var ge GateError
		if !errors.As(err, &ge) || ge.Status != http.StatusForbidden {
			t.Fatalf("err = %v, want 403 GateError", err)
		}
	})

	t.Run("bad merchant id -> 401", func(t *testing.T) {
		p := *base
		p.MerchantID = "not-a-merchant-id"
		g := NewDelegatedGate(DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*DelegatedPrincipal, error) {
			return &p, nil
		}))
		_, err := g.Authorize(context.Background(), req, "billing:read")
		var ge GateError
		if !errors.As(err, &ge) || ge.Status != http.StatusUnauthorized {
			t.Fatalf("err = %v, want 401 GateError", err)
		}
	})

	t.Run("success maps the principal", func(t *testing.T) {
		g := NewDelegatedGate(DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*DelegatedPrincipal, error) {
			p := *base
			return &p, nil
		}))
		got, err := g.Authorize(context.Background(), req, "billing:read")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.UserContext.UserID != "user-1" || got.UserContext.Merchant != "doujins" {
			t.Fatalf("principal mapping wrong: %+v", got)
		}
	})
}
