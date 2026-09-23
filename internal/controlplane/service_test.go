package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/open-rails/openrails/config"
)

// testEd25519PrivateKeyPEM generates a throwaway ed25519 private key PEM
// (PKCS8), the smallest key type resolveControlPlaneKeySource's
// jwtkit.NewStaticKeySourceFromPEM accepts.
func testEd25519PrivateKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8 key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// TestResolveControlPlaneKeySource_InlinePEMWarnsOutsideDev pins #752: inline
// ACTIVE_PRIVATE_KEY_PEM is frozen for the process lifetime (no hot rotation,
// no emergency revocation without a restart) — a non-development boot on this
// path must say so loudly.
func TestResolveControlPlaneKeySource_InlinePEMWarnsOutsideDev(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			ActiveKeyID:         "test-key",
			ActivePrivateKeyPEM: testEd25519PrivateKeyPEM(t),
		},
	}

	hook := logtest.NewGlobal()
	defer hook.Reset()

	if _, err := resolveControlPlaneKeySource(cfg); err != nil {
		t.Fatalf("resolveControlPlaneKeySource: %v", err)
	}

	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "inline PEM") && strings.Contains(e.Message, "FROZEN") {
			warned = true
		}
	}
	if !warned {
		t.Fatal("expected a non-development inline-PEM rotation-tradeoff warning")
	}
}

func TestResolveControlPlaneKeySource_EphemeralRequiresExplicitPermission(t *testing.T) {
	cfg := &config.Config{Auth: &config.AuthConfig{KeysPath: t.TempDir()}}
	if _, err := resolveControlPlaneKeySource(cfg); err == nil {
		t.Fatal("missing key must fail closed")
	}
	cfg.Auth.AllowEphemeralSigningKey = true
	if _, err := resolveControlPlaneKeySource(cfg); err != nil {
		t.Fatal(err)
	}
}

// TestResolveControlPlaneKeySource_KeysPathNoWarning: the keys_path/keys.json
// delivery route is the hot-rotating prod path — it must never trip the #752
// inline-PEM warning, even outside development.
func TestResolveControlPlaneKeySource_KeysPathNoWarning(t *testing.T) {
	cfg := &config.Config{
		Auth: &config.AuthConfig{
			KeysPath: t.TempDir(),
		},
	}

	hook := logtest.NewGlobal()
	defer hook.Reset()

	// No keys.json present and not dev -> ResolveKeySource is expected to
	// error; only the ABSENCE of the inline-PEM warning is under test here.
	_, _ = resolveControlPlaneKeySource(cfg)

	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "inline PEM") {
			t.Fatalf("unexpected inline-PEM warning for keys_path delivery: %q", e.Message)
		}
	}
}

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

func TestDelegatedRequestURLUsesTrustedOriginAndEscapedTarget(t *testing.T) {
	for _, path := range []string{"/v1/orders", "/billing/v1/orders", "/billing/v1/a%2Fb", "/billing/v1/a//b"} {
		r := httptest.NewRequest(http.MethodGet, "http://untrusted.test"+path+"?q=x", nil)
		r.Header.Set("X-Forwarded-Host", "attacker.test")
		got := delegatedRequestURL("https://billing.example", nil)(r)
		if got != "https://billing.example"+path {
			t.Fatalf("target %q: %q", path, got)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "http://untrusted.test/billing/v1/a", nil)
	for _, bad := range []string{"", "https://billing.example/billing", "https://user:secret@billing.example", "https://billing.example?override=yes"} {
		if got := delegatedRequestURL(bad, nil)(r); got != "" {
			t.Fatalf("invalid origin %q produced %q", bad, got)
		}
	}
	if got := delegatedRequestURL("", func(*http.Request) string { return "https://host.example/original%2Fpath" })(r); got != "https://host.example/original%2Fpath" {
		t.Fatal(got)
	}
}
