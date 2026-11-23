package nmimock

import (
	"reflect"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/mirror"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A vault reaches the gateway whole.
func TestMockVaultCopiesEveryField(t *testing.T) {
	var v Vault
	mirror.Fill(&v)
	v.Created = time.Unix(1, 0)
	for _, field := range mirror.Zero(mockVault(v), "github.com/open-rails/openrails/internal/nmimock") {
		t.Errorf("mockVault leaves %s zero", field)
	}
	for _, field := range mirror.Missing(reflect.TypeFor[nmimock.Vault](), reflect.TypeFor[Vault]()) {
		t.Errorf("%s has no field in nmimock.Vault", field)
	}
}
