package openrails

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/userinfo"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/mirror"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

const (
	rootPkg   = "github.com/open-rails/openrails"
	configPkg = "github.com/open-rails/openrails/internal/config"
)

// The values Config's fields take are the engine's.
func TestConfigValuesAreTheEngines(t *testing.T) {
	for public, engine := range map[string]string{
		string(billing.EnvironmentLive): config.EnvironmentLive, string(billing.EnvironmentTest): config.EnvironmentTest, string(billing.EnvironmentSandbox): config.EnvironmentSandbox,
		string(billing.EnvironmentMainnet): config.EnvironmentMainnet, string(billing.EnvironmentDevnet): config.EnvironmentDevnet, string(billing.EnvironmentTestnet): config.EnvironmentTestnet,
		QueueBilling: riverjobs.QueueBilling, QueuePSPImport: riverjobs.QueuePSPImport,
	} {
		if public != engine {
			t.Errorf("%q is %q in the engine", public, engine)
		}
	}
}

// Every read the route catalog caches has a remote policy, the widest.
func TestCatalogReadsHavePolicies(t *testing.T) {
	for _, r := range httproutes.Catalog() {
		if _, ok := readPolicies[overHTTP][r.Cache]; r.Cache != "" && !ok {
			t.Errorf("%s %s caches %q, which has no policy", r.Method, r.Path, r.Cache)
		}
	}
}

type stub struct {
	billing.MerchantGroupReader
	userinfo.Lookup
	notify.Publisher
}

type clickHouseStub struct{ driver.Conn }

type emailCapture struct{ sent *Email }

func (c emailCapture) Send(_ context.Context, m Email) error { *c.sent = m; return nil }
func (emailCapture) CheckHealth(context.Context) error       { return nil }

// Every Deps field reaches the engine, and the host's hooks see every field
// the engine hands them.
func TestEngineDepsCopiesEveryField(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{})
	defer redisClient.Close()
	var sent Email
	var recipient NotificationRecipient
	deps := Deps{
		Postgres: &pgxpool.Pool{}, PostgresSession: &pgxpool.Config{}, MerchantGroups: stub{}, Redis: redisClient, Vault: &vaultapi.Client{}, ClickHouse: clickHouseStub{},
		Email: emailCapture{&sent}, Logger: slog.New(slog.DiscardHandler), UserInfo: stub{},
		MessageRenderer: func(context.Context, billing.MessageData) (billing.MessageContent, bool, error) {
			return billing.MessageContent{}, false, nil
		},
		Notifications: &NotificationChannel{Sink: stub{}, Recipient: func(_ context.Context, r NotificationRecipient) (string, bool, error) {
			recipient = r
			return "account", true, nil
		}},
		CheckoutAssets:  fstest.MapFS{"index.html": {}},
		StripeTransport: http.DefaultTransport, BasisTheoryTransport: http.DefaultTransport, NMITransport: http.DefaultTransport, FXTransport: http.DefaultTransport,
		Clock: clockwork.NewFakeClock(),
		ProviderEndpoints: ProviderEndpoints{NMIGatewayURL: "http://127.0.0.1:1", StripeAPIURL: "http://127.0.0.1:2", SolanaRPCURL: "http://127.0.0.1:3",
			CCBillDataLinkURL: "http://127.0.0.1:4", CCBillWebhookSources: []string{"127.0.0.1/32"}},
	}
	for _, field := range mirror.Zero(deps, rootPkg) {
		t.Fatalf("the test leaves %s zero", field)
	}
	out := engineDeps(deps)
	for _, field := range mirror.Zero(out, rootPkg+"/internal/engine", rootPkg+"/internal/app", configPkg) {
		if field != "Deps.BootstrapOptions.Migrations" { // a test seam of the engine's own
			t.Errorf("engineDeps leaves %s zero", field)
		}
	}
	var m config.Email
	mirror.Fill(&m)
	if err := out.EmailSender.Send(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	for _, field := range mirror.Zero(sent, rootPkg) {
		t.Errorf("the host's EmailSender gets %s zero", field)
	}
	var r config.NotificationRecipient
	mirror.Fill(&r)
	if _, _, err := out.Notifications.Recipient(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	for _, field := range mirror.Zero(recipient, rootPkg) {
		t.Errorf("the host's Recipient gets %s zero", field)
	}
	if none := engineDeps(Deps{}); none.EmailSender != nil || none.Notifications != nil {
		t.Error("an unset hook must stay nil")
	}
}

// Routes and CustomerRoutes reach the engine whole.
func TestEngineRoutesCopiesEveryField(t *testing.T) {
	routes := Routes{Auth: struct{ auth.Authenticator }{}, Prefix: "/billing"}
	mirror.Fill(&routes.RouteGroups)
	for _, field := range mirror.Zero(engineRoutes(routes), configPkg) {
		t.Errorf("engineRoutes leaves %s zero", field)
	}
	want := CustomerBinding{MerchantID: billing.MerchantID{1}, CustomerID: billing.CustomerID{2}}
	customer := engineCustomerRoutes(CustomerRoutes{Prefix: "/me", Auth: routes.Auth, Resolve: func(*http.Request, auth.Verified) (CustomerBinding, error) { return want, nil }})
	for _, field := range mirror.Zero(customer, configPkg) {
		t.Errorf("engineCustomerRoutes leaves %s zero", field)
	}
	if got, _ := customer.Resolve(nil, nil); CustomerBinding(got) != want {
		t.Errorf("Resolve answered %+v", got)
	}
	missing := append(mirror.Missing(reflect.TypeFor[Routes](), reflect.TypeFor[config.Routes]()),
		mirror.Missing(reflect.TypeFor[CustomerRoutes](), reflect.TypeFor[config.CustomerRoutes]())...)
	for _, field := range missing {
		t.Errorf("%s has no field in the engine's", field)
	}
}
