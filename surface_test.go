package openrails

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/http/routes"
)

// The Go client covers merchant operations: every regular merchant route
// has exactly one Client method and each method one route. Optional console
// assistance explicitly declares ConsoleOnly; the lists below name local
// helpers and embedded-only operations with their reasons.

// handleMethods say what a handle is or ask the token endpoint, which is no
// merchant route.
var handleMethods = map[string]string{
	"EventReader":       "constructs the shared reader adapter; polling calls ListEvents",
	"MerchantID":        "the handle's merchant",
	"CustomerToken":     "mints a customer's access token at the issuer's token endpoint",
	"CustomerAssertion": "signs the assertion a frontend redeems for that token",
}

// engineMethods are everything an Engine has: its lifecycle, the HTTP surface
// it hands its host, and its merchants' handles. No merchant's operation is
// on it.
var engineMethods = map[string]string{
	"CheckoutPage":             "configures the isolated hosted-order surface before startup",
	"Start":                    "starts the workers",
	"Close":                    "stops the engine",
	"Ready":                    "the readiness probe",
	"Probes":                   "the optional dependencies, for the host's supervisor",
	"Routes":                   "the HTTP surface the host mounts",
	"PublicConfig":             "what a mount's GET /v1/config serves, for the host's own page response",
	"CustomerConfig":           "what a mount's GET /v1/me/config serves a signed-in customer, for the host's own page response",
	"RiverJobs":                "the jobs a host-owned River fleet runs",
	"Merchant":                 "a merchant's handle",
	"LocalMerchant":            "an embedded extension of a merchant's handle",
	"MerchantBySlug":           "the handle of a merchant Config declares",
	"ListOpenDestructiveHolds": "deployment-wide incidents for the trusted host operator",
	"ClearDestructiveHold":     "a trusted operator clears an incident that can span merchants",
}

// remoteMethods are everything a Remote has: nothing spans its merchants.
var remoteMethods = map[string]string{
	"Ready":    "the server's /health/ready",
	"Close":    "releases idle connections",
	"Merchant": "a merchant's handle",
}

// embeddedMethods are operations only LocalClient offers. Each one
// serves a hosted product or runs inside a transaction of the host's, so no
// merchant route can carry it; the reason names the host that calls it. The
// control plane's cross-merchant operations are package server's.
var embeddedMethods = map[string]string{
	"SCIMHandler":          "the SCIM service provider an in-process directory pushes to",
	"DeclarePSP":           "the hosted product: a PSP identity without credentials, for imported billing facts",
	"StartRunningChargeTx": "host-four: StartRunningCharge inside the host's transaction",
	"StopRunningChargeTx":  "host-four: StopRunningCharge inside the host's transaction",
	"AdmitTx":              "host-four: Admit inside the host's transaction",
	"CaptureAdmissionsTx":  "host-four: CaptureAdmissions inside the host's transaction",
	"ReleaseAdmissionsTx":  "host-four: ReleaseAdmissions inside the host's transaction",
}

// routeArguments are the arguments of the methods that refuse a request with
// every field set (they take one of several).
var routeArguments = map[string][]any{
	"ApplyCatalog":          {&catalog.Application{SchemaVersion: catalog.ApplicationSchemaVersion}, billing.ApplyCatalogParams{}},
	"CreatePrice":           {billing.CreatePriceParams{ProductKey: "pro", Currency: "USD", UnitAmount: 1}},
	"CreateProviderPrice":   {billing.CreateProviderPriceParams{ProductKey: "pro", Key: "legacy", PSP: "nmi", PlanID: "plan"}},
	"SetMeter":              {"tokens", billing.SetMeterParams{Aggregation: catalog.AggregationSum, RateCard: catalog.Null[billing.MeterRateCardParams]()}},
	"RefundPayment":         {billing.PaymentID(uuid.New()), billing.RefundPaymentParams{Full: true, Reason: "requested", IdempotencyKey: "k"}},
	"CreatePriceMigration":  {billing.CreatePriceMigrationParams{FromPriceID: billing.PriceID("test:" + uuid.NewString() + ":v1"), ToPriceID: billing.PriceID("test:" + uuid.NewString() + ":v1")}},
	"PreviewPriceMigration": {billing.CreatePriceMigrationParams{ProductKey: "pro", PriceKey: "monthly"}},
	"UpdatePrice":           {billing.PriceID("test:" + uuid.NewString() + ":v1"), billing.UpdatePriceParams{Archived: catalog.Value(true)}},
	"UpdateProduct":         {billing.ProductID(uuid.New()), billing.UpdateProductParams{Archived: catalog.Value(true)}},
	"UpdateAlertWebhook":    {billing.AlertWebhookID(uuid.New()), billing.UpdateAlertWebhookParams{Enabled: catalog.Value(true)}},
	"UpdateCustomer":        {billing.CustomerID(uuid.New()), billing.UpdateCustomerParams{BillingPolicy: catalog.Null[string]()}},
}

// verbs are the first words a method on each HTTP method may start with; a
// POST is a create or an action named for what it does.
var verbs = map[string][]string{
	http.MethodGet:    {"Get", "List", "Export"},
	http.MethodPut:    {"Set", "Ensure"},
	http.MethodPatch:  {"Update"},
	http.MethodDelete: {"Delete"},
}

// documents are the requests that are not named ...Params: what the route
// stores or runs, sent whole.
var documents = []string{"DeclaredBilling", "MetricsQuery", "EventQuery", "PageRequest", "Application"}

// synonyms are spellings of the seven verbs the API does not use.
var synonyms = []string{"Retrieve", "Fetch", "Read", "Find", "Lookup", "Search", "Upsert", "Put", "Add", "New", "Make", "Remove", "Destroy", "Modify", "Edit", "Patch"}

// fill sets v so that a method's own checks pass: every field set.
func fill(v reflect.Value, depth int) {
	switch v.Interface().(type) {
	case billing.PriceID:
		v.Set(reflect.ValueOf(billing.PriceID("test:price:v1")))
		return
	case time.Time:
		v.Set(reflect.ValueOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
		return
	case json.RawMessage:
		v.Set(reflect.ValueOf(json.RawMessage(`{}`)))
		return
	}
	if depth > 6 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Array:
		for i := range v.Len() {
			fill(v.Index(i), depth+1)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), depth+1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, depth+1)
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Field(i); f.CanSet() {
				fill(f, depth+1)
			}
		}
	case reflect.Interface:
		switch v.Type() {
		case reflect.TypeFor[io.Writer]():
			v.Set(reflect.ValueOf(&bytes.Buffer{}))
		case reflect.TypeFor[io.Reader]():
			v.Set(reflect.ValueOf(strings.NewReader("{}")))
		}
	}
}

// surfaceProbe calls Client methods against a transport that records each
// request as the catalog route it addresses.
type surfaceProbe struct {
	t      *testing.T
	client *Client
	mux    *http.ServeMux
	seen   []string
}

func newSurfaceProbe(t *testing.T) *surfaceProbe {
	p := &surfaceProbe{t: t, mux: http.NewServeMux()}
	for _, r := range routes.Catalog() {
		p.mux.HandleFunc(r.Key(), func(http.ResponseWriter, *http.Request) {})
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_, pattern := p.mux.Handler(r)
		if pattern == "" {
			pattern = r.Method + " " + r.URL.Path + " (no such route)"
		}
		p.seen = append(p.seen, pattern)
		return okResponse(`{}`), nil
	})
	p.client = remoteHandle(t, "http://openrails.invalid", RemoteOptions{
		Credentials: map[billing.MerchantID]RemoteCredential{testMerchant: {APIKey: "k"}},
		HTTPClient:  &http.Client{Transport: transport},
	})
	return p
}

// call invokes a method on client and returns the routes it requested and
// its error.
func (p *surfaceProbe) call(client *Client, m reflect.Method) ([]string, error) {
	args := []reflect.Value{reflect.ValueOf(client)}
	if given, ok := routeArguments[m.Name]; ok {
		args = append(args, reflect.ValueOf(p.t.Context()))
		for _, arg := range given {
			args = append(args, reflect.ValueOf(arg))
		}
	} else {
		for i := 1; i < m.Type.NumIn(); i++ {
			in := m.Type.In(i)
			if m.Type.IsVariadic() && i == m.Type.NumIn()-1 {
				break
			}
			if in == reflect.TypeFor[context.Context]() {
				args = append(args, reflect.ValueOf(p.t.Context()))
				continue
			}
			v := reflect.New(in).Elem()
			fill(v, 0)
			args = append(args, v)
		}
	}
	p.seen = nil
	out := m.Func.Call(args)
	var err error
	if n := len(out); n > 0 {
		err, _ = out[n-1].Interface().(error)
	}
	return p.seen, err
}

func TestClientIsTheMerchantAPI(t *testing.T) {
	p := newSurfaceProbe(t)

	catalogRoutes := map[string]routes.Route{}
	for _, r := range routes.Catalog() {
		catalogRoutes[r.Key()] = r
	}
	methodsOf := map[string][]string{} // route -> methods
	ct := reflect.TypeOf(p.client)
	names := map[string]bool{}
	for i := range ct.NumMethod() {
		m := ct.Method(i)
		names[m.Name] = true
		if _, ok := handleMethods[m.Name]; ok {
			require.NotContains(t, embeddedMethods, m.Name)
			continue
		}
		seen, err := p.call(p.client, m)
		if len(seen) != 1 {
			t.Errorf("%s must call exactly one route, not %v (error: %v); otherwise list it with its reason", m.Name, seen, err)
			continue
		}
		route, ok := catalogRoutes[seen[0]]
		if !ok || !clientRoute(route) {
			t.Errorf("%s calls %s, which is not a merchant route", m.Name, seen[0])
			continue
		}
		if route.Name != m.Name {
			t.Errorf("%s calls %s, which the catalog names %s (its Client method)", m.Name, seen[0], route.Name)
		}
		methodsOf[seen[0]] = append(methodsOf[seen[0]], m.Name)

		if allowed, ok := verbs[route.Method]; ok && !slices.ContainsFunc(allowed, func(verb string) bool { return strings.HasPrefix(m.Name, verb) }) {
			t.Errorf("%s is a %s (%s): its name starts with one of %v", m.Name, route.Method, route.Path, allowed)
		}
		for _, synonym := range synonyms {
			if strings.HasPrefix(m.Name, synonym) {
				t.Errorf("%s: say Get, List, Create, Update, Delete, Set or Ensure, not %s", m.Name, synonym)
			}
		}

		// A request struct is named ...Params; a result is a pointer, a
		// map or a list, never a struct by value.
		for i := 2; i < m.Type.NumIn(); i++ {
			in := m.Type.In(i)
			for in.Kind() == reflect.Pointer || in.Kind() == reflect.Slice {
				in = in.Elem()
			}
			if in.Kind() == reflect.Struct && strings.HasPrefix(in.PkgPath(), "github.com/open-rails/openrails/") && !strings.HasSuffix(in.Name(), "Params") && !slices.Contains(documents, in.Name()) {
				t.Errorf("%s takes %s: name a request struct ...Params", m.Name, in.Name())
			}
		}
		if out := m.Type.Out(0); out.Kind() == reflect.Struct {
			t.Errorf("%s returns %s by value: return a pointer", m.Name, out)
		}
	}
	for name := range handleMethods {
		require.True(t, names[name], "handleMethods lists %s, which Client does not have", name)
	}
	for name := range embeddedMethods {
		require.False(t, names[name], "embeddedMethods lists %s, which must not be on Client", name)
	}

	for _, r := range routes.Catalog() {
		switch {
		case clientRoute(r):
			methods := methodsOf[r.Key()]
			sort.Strings(methods)
			if len(methods) != 1 {
				t.Errorf("%s has %d Client methods %v; a merchant route has exactly one", r.Key(), len(methods), methods)
			}
		default:
			if len(methodsOf[r.Key()]) > 0 {
				t.Errorf("%s is a %s route: no Client method, not %v", r.Key(), r.Group, methodsOf[r.Key()])
			}
		}
	}
}

// What spans merchants is on the Engine and the Remote, and nothing else is:
// a merchant's operations are its handle's alone.
func TestEngineAndRemoteCarryNoMerchantOperation(t *testing.T) {
	for typ, want := range map[reflect.Type]map[string]string{
		reflect.TypeFor[*Engine](): engineMethods,
		reflect.TypeFor[*Remote](): remoteMethods,
	} {
		var got []string
		for i := range typ.NumMethod() {
			got = append(got, typ.Method(i).Name)
		}
		require.ElementsMatch(t, slices.Collect(maps.Keys(want)), got, "%s: a merchant's operation is a Client method; list anything else with its reason", typ)
	}
}

// clientRoute reports a route the Client has a method for.
func clientRoute(r routes.Route) bool { return r.ClientMethod() }
