package main

import (
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/mirror"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

// The loaded naming and admin_console sections reach server.Config whole.
func TestServerConfigSectionsCopyEveryField(t *testing.T) {
	var naming config.NamingConfig
	mirror.Fill(&naming)
	for _, field := range mirror.Zero(serverNaming(naming), "github.com/open-rails/openrails/server") {
		t.Errorf("serverNaming leaves %s zero", field)
	}
	var console hostconfig.ConsoleConfig
	mirror.Fill(&console)
	for _, field := range mirror.Zero(serverConsole(&console), "github.com/open-rails/openrails/server") {
		t.Errorf("serverConsole leaves %s zero", field)
	}
}
