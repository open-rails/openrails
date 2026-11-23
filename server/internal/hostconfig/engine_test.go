package hostconfig

import (
	"reflect"
	"testing"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/mirror"
)

// Every loaded engine field reaches the openrails.Config the server runs,
// and that Config has no field the loader cannot fill but ReadCache.
func TestEngineConfigCopiesEveryField(t *testing.T) {
	var cfg config.Config
	mirror.Fill(&cfg)
	for _, field := range mirror.Zero(EngineConfig(&cfg), "github.com/open-rails/openrails") {
		if field != "Config.ReadCache" {
			t.Errorf("EngineConfig leaves %s zero", field)
		}
	}
	for _, field := range mirror.Missing(reflect.TypeFor[config.Config](), reflect.TypeFor[openrails.Config]()) {
		t.Errorf("%s has no field in openrails.Config", field)
	}
}
