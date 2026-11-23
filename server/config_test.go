package server

import (
	"reflect"
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/mirror"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// The server's own configuration types reach its internals whole, and their
// values are the internals'.
func TestServerConfigCopiesEveryField(t *testing.T) {
	var naming NamingConfig
	mirror.Fill(&naming)
	for _, field := range mirror.Zero(namingConfig(naming), "github.com/open-rails/openrails/internal/config") {
		t.Errorf("namingConfig leaves %s zero", field)
	}
	var console AdminConsole
	mirror.Fill(&console)
	for _, field := range mirror.Zero(consoleConfig(&console), "github.com/open-rails/openrails/server/internal/hostconfig") {
		t.Errorf("consoleConfig leaves %s zero", field)
	}
	missing := append(mirror.Missing(reflect.TypeFor[NamingConfig](), reflect.TypeFor[config.NamingConfig]()),
		mirror.Missing(reflect.TypeFor[AdminConsole](), reflect.TypeFor[hostconfig.ConsoleConfig]())...)
	for _, field := range missing {
		t.Errorf("%s has no field in the server's internals", field)
	}
	for public, internal := range map[string]string{
		ConsoleClientID:              controlplane.ConsoleClientID,
		string(FormerNamesFinite):    string(config.FormerNamesFinite),
		string(FormerNamesForever):   string(config.FormerNamesForever),
		string(FormerNamesImmediate): string(config.FormerNamesImmediate),
	} {
		if public != internal {
			t.Errorf("%q is %q inside the server", public, internal)
		}
	}
}
