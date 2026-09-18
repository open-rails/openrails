//go:build integration

package integrationharness

import (
	"context"
	"errors"
	embeddedapi "github.com/open-rails/openrails/pkg/embedded"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// moneyDeployment is one deployment shape under test, restartable on the same
// database. The workflow program below only ever sees client().
type moneyDeployment struct {
	name     string
	merchant merchant.ID
	// runtime is the graph fixtures are seeded through (never the code under
	// test's own client path).
	runtime func() *app.Runtime
	client  func() *openrails.Client
	stop    func()
	start   func()
}

// moneyDeploymentBuilders builds, per subtest, one of the three shapes against
// one loopback NMI gateway: embedded (runtime object graph in this process),
// standalone (the actual cmd/openrails server as a separate OS process) and
// saas (the shared multi-merchant engine behind the hosted control plane).
// Exactly one deployment runs workers at a time, so a converged operation was
// converged by the deployment under test.
func moneyDeploymentBuilders(h *Harness, gateway *FakeNMIGateway) []struct {
	name  string
	build func(t *testing.T) moneyDeployment
} {
	ctx := context.Background()
	sandbox := func(cfg *config.Config) {
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
	}
	return []struct {
		name  string
		build func(t *testing.T) moneyDeployment
	}{
		{"embedded", func(t *testing.T) moneyDeployment {
			var embedded *embed.Runtime
			start := func() {
				cfg := &config.Config{
					Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI,
					SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN},
				}
				sandbox(cfg)
				rt, err := embed.New(ctx, embed.Options{Options: embeddedapi.Options{Config: cfg, Redis: h.Redis, River: embeddedapi.RiverManagedByOpenRails()}, RunWorkers: true})
				require.NoError(t, err)
				embedded = rt
			}
			stop := func() {
				if embedded == nil {
					return
				}
				require.NoError(t, embedded.Close(context.Background()))
				embedded = nil
			}
			dbtest.EnsureTestMerchant(ctx, t, h.Pool())
			start()
			t.Cleanup(stop)
			return moneyDeployment{
				name: "embedded", merchant: dbtest.TestMerchantID,
				runtime: func() *app.Runtime { return app.HostGraph(embedded).Runtime },
				client: func() *openrails.Client {
					client, err := embedded.Client(openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"), openrails.WithTimeout(30*time.Second))
					require.NoError(t, err)
					return client
				},
				stop: stop, start: start,
			}
		}},
		{"standalone", func(t *testing.T) moneyDeployment {
			// The in-process standalone graph seeds fixtures and mints the
			// API key; it runs no workers. The code under test is the process.
			standalone := h.StartStandalone("USD")
			process := h.StartStandaloneProcess(ProcessWithNMIGateway(gateway.URL))
			t.Cleanup(process.Stop)
			return moneyDeployment{
				name: "standalone", merchant: dbtest.TestMerchantID,
				runtime: func() *app.Runtime { return standalone.App().Runtime },
				client: func() *openrails.Client {
					client, err := openrails.NewRemote(process.BaseURL, openrails.WithAPIKey(standalone.Token), openrails.WithMerchantID(dbtest.TestMerchantID), openrails.WithCurrency("USD"), openrails.WithTimeout(30*time.Second))
					require.NoError(t, err)
					return client
				},
				stop: process.Kill, start: process.Start,
			}
		}},
		{"saas", func(t *testing.T) moneyDeployment {
			hosted := h.StartHosted("USD", HostedWithWorkers(), HostedWithConfig(sandbox))
			t.Cleanup(hosted.Stop)
			owner := hosted.RegisterUser("owner")
			tenant := hosted.ProvisionMerchant(owner, "money-"+uuid.NewString()[:8])
			return moneyDeployment{
				name: "saas", merchant: tenant.ID,
				runtime: hosted.AppRuntime,
				client:  func() *openrails.Client { return tenant.Client() },
				stop:    hosted.Stop, start: hosted.Start,
			}
		}},
	}
}

// runMoneyDeployments runs program once per deployment shape, each built and
// torn down inside its own subtest.
func runMoneyDeployments(t *testing.T, h *Harness, gateway *FakeNMIGateway, program func(t *testing.T, d moneyDeployment)) {
	t.Helper()
	parent := t
	// The shared fixture pool and the server binary outlive every subtest.
	h.Pool()
	_, err := h.openrailsBinary()
	require.NoError(t, err)
	for _, b := range moneyDeploymentBuilders(h, gateway) {
		t.Run(b.name, func(t *testing.T) {
			h.SetT(t)
			t.Cleanup(func() { h.SetT(parent) })
			program(t, b.build(t))
		})
	}
}

func requireRefusal(t *testing.T, err error, sentinel error, code string) {
	t.Helper()
	require.ErrorIs(t, err, sentinel)
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "%v", err)
	require.Equal(t, code, status.Code)
}
