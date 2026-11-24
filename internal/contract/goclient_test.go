package contract

import (
	"flag"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/http/routes"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/goclient.golden")

// goldenRoutes are one route of each shape the generator writes.
var goldenRoutes = []routes.Route{
	{Method: routes.GET, Path: "/v1/catalog/meters/{key}", Group: routes.CatalogAdmin, Auth: routes.AuthMerchant, Name: "GetMeter", Level: routes.LevelRead,
		Args: []any{""}, Cache: billing.ReadMeters, Doc: "GetMeter reads one meter with its rate card.",
		Responses: []routes.Reply{{Status: 200, Body: billing.Meter{}}}},
	{Method: routes.GET, Path: "/v1/admin/customers/{customer_id}/balance", Group: routes.Admin, Auth: routes.AuthMerchant, Name: "GetBalance", Level: routes.LevelRead,
		Args: []any{billing.CustomerID{}}, Query: billing.GetBalanceParams{}, Doc: "GetBalance reads a customer's balance in params.Currency.",
		Responses: []routes.Reply{{Status: 200, Body: billing.Balance{}}}},
	{Method: routes.POST, Path: "/v1/admin/payments/{id}/refunds", Group: routes.Admin, Auth: routes.AuthMerchant, Name: "RefundPayment", Level: routes.LevelUpdate, IdempotencyKey: true,
		Args: []any{billing.PaymentID{}}, Request: billing.RefundPaymentParams{},
		Doc:       "RefundPayment refunds a charge through its rail.\n\nA retry with idempotencyKey returns the same refund.",
		Responses: []routes.Reply{{Status: 201, Body: billing.Payment{}}, {Status: 202, Body: billing.Payment{}}}},
	{Method: routes.POST, Path: "/v1/app/leases/sync", Group: routes.App, Auth: routes.AuthApplication, Name: "SyncLeases",
		Request: billing.SyncLeasesParams{}, Doc: "SyncLeases settles a lease's usage and answers its views.",
		Responses: []routes.Reply{{Status: 200, Body: billing.SyncLeasesResult{}}}},
	{Method: routes.POST, Path: "/v1/catalog/apply", Group: routes.CatalogAdmin, Auth: routes.AuthMerchant, Name: "ApplyCatalog", Level: routes.LevelUpdate,
		Request: &catalog.Application{}, Query: billing.ApplyCatalogParams{}, Doc: "ApplyCatalog applies a catalog document; params.Force overwrites edits.",
		Responses: []routes.Reply{{Status: 200, Body: billing.CatalogApplicationReceipt{}}}},
	{Method: routes.DELETE, Path: "/v1/admin/customers/{customer_id}/payment-methods/{id}", Group: routes.Admin, Auth: routes.AuthMerchant, Name: "DeletePaymentMethod", Level: routes.LevelUpdate,
		Args: []any{billing.CustomerID{}, billing.PaymentMethodID{}}, Doc: "DeletePaymentMethod removes an unused card.",
		Responses: []routes.Reply{{Status: 204}}},
}

// TestGoClientGolden pins the generated method of each route shape.
func TestGoClientGolden(t *testing.T) {
	imports := map[string]bool{}
	var b strings.Builder
	for _, r := range goldenRoutes {
		method, err := clientMethod(r, imports)
		if err != nil {
			t.Fatalf("%s: %v", r.Key(), err)
		}
		b.WriteString(method)
	}
	const golden = "testdata/goclient.golden"
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("generated methods differ from %s (go test -run TestGoClientGolden -update):\n%s", golden, b.String())
	}
}

// TestGoClientDeclaresHandWritten refuses an exported Client method that is
// neither generated nor declared.
func TestGoClientDeclaresHandWritten(t *testing.T) {
	var methods strings.Builder
	methods.WriteString("package openrails\n")
	for _, name := range handWrittenMethods {
		methods.WriteString("func (c *Client) " + name + "() {}\n")
	}
	ok := fstest.MapFS{handWrittenFile: {Data: []byte(methods.String())}}
	if err := checkHandWritten(ok, handWrittenMethods); err != nil {
		t.Fatalf("declared methods refused: %v", err)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"a method elsewhere":   {handWrittenFile: ok[handWrittenFile], "orders.go": {Data: []byte("package openrails\nfunc (c *Client) GetOrder() {}\n")}},
		"an undeclared method": {handWrittenFile: {Data: []byte(methods.String() + "func (c *Client) Extra() {}\n")}},
		"a missing method":     {handWrittenFile: {Data: []byte("package openrails\n")}},
	} {
		if err := checkHandWritten(fsys, handWrittenMethods); err == nil {
			t.Errorf("%s is accepted", name)
		}
	}
}
