package embedded

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
)

func TestCatalogPushRuntimeReusesProvidedRuntime(t *testing.T) {
	hostRuntime := &app.Runtime{
		MoneyService:       &money.MoneyService{},
		EntitlementService: &entitlements.EntitlementService{},
	}
	host := &Embedded{app: &app.App{Runtime: hostRuntime}}

	rt, svc, cleanup, err := catalogPushRuntime(context.Background(), CatalogPushOptions{Runtime: host})
	if err != nil {
		t.Fatalf("catalogPushRuntime: %v", err)
	}
	t.Cleanup(cleanup)
	if rt != hostRuntime {
		t.Fatal("catalog push must preserve the host runtime and its armed merchant credential plane")
	}
	if svc == nil {
		t.Fatal("catalog push service is nil")
	}
}
