package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/server/internal/controlplane"
)

// PrivateNetworkApplications reports whether an application the operator
// declares reads its keys off the public internet: an http, loopback or
// private jwks_uri, or with no keys and no jwks_uri such an issuer, whose
// metadata names them. It reads the boot merchant manifest (merchantPath,
// else the conventional one) and the bootstrap manifest; a missing
// conventional file declares nothing. The server's AuthKit then fetches keys
// on a private network; applications registered at run time still may not
// name one (controlplane.RegistrationGuard).
func PrivateNetworkApplications(merchantPath string, logger *slog.Logger) (bool, error) {
	path := strings.TrimSpace(merchantPath)
	if path == "" {
		path = DefaultMerchantConfigManifestPath
	}
	if raw, err := os.ReadFile(path); err == nil { // #nosec G304 -- boot-time operator path
		manifest, err := LoadMerchantConfigManifest(raw)
		if err != nil {
			return false, fmt.Errorf("merchant manifest %s: %w", path, err)
		}
		for _, m := range manifest.Merchants {
			if app := m.RemoteApplication; app != nil && private(app.Issuer, app.JWKSURI, len(app.PublicKeys) > 0 || len(app.JWKS.Keys) > 0) {
				return true, nil
			}
		}
	} else if !os.IsNotExist(err) || merchantPath != "" {
		return false, fmt.Errorf("read merchant manifest %s: %w", path, err)
	}
	raw, err := os.ReadFile(DefaultBootstrapManifestPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read AuthKit authority manifest: %w", err)
	}
	manifest, err := authkit.ParseBootstrapManifestYAML(raw, logger)
	if err != nil {
		return false, fmt.Errorf("AuthKit authority manifest %s: %w", DefaultBootstrapManifestPath, err)
	}
	for _, app := range manifest.RemoteApplications {
		if private(app.Issuer, app.JWKSURI, len(app.PublicKeys) > 0) {
			return true, nil
		}
	}
	return false, nil
}

// private: the keys of an application with these trust sources are read off
// the public internet.
func private(issuer, jwksURI string, static bool) bool {
	switch {
	case strings.TrimSpace(jwksURI) != "":
		return controlplane.PublicHTTPS(jwksURI) != nil
	case static:
		return false
	}
	return controlplane.PublicHTTPS(issuer) != nil
}
