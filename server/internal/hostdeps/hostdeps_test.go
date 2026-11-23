package hostdeps

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/openrails/server/internal/hostconfig"
)

func TestVaultReadsTheCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := Vault(context.Background(), &hostconfig.VaultLogin{Address: srv.URL, Token: "t", CACert: path})
	if err != nil {
		t.Fatal(err)
	}
	session.Close()

	_, err = Vault(context.Background(), &hostconfig.VaultLogin{Address: srv.URL, Token: "t", CACert: filepath.Join(t.TempDir(), "missing.pem")})
	if err == nil || !strings.Contains(err.Error(), "vault.ca_cert") {
		t.Fatalf("an unreadable CA file: %v", err)
	}
}
