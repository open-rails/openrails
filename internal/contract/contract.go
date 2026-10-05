// Package contract renders the route catalog and the error-code registry as
// the files other tools read: the OpenAPI document, the TypeScript wire types
// of the browser SDK and the console, and the route and error-code tables of
// the API reference. They are committed; Verify fails when one is stale.
package contract

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/http/routes"
)

// Where each generated file lives, relative to the repository.
const (
	OpenAPIFile = "api/openapi.json"
	RoutesDoc   = "docs/api/routes.md"
	CodesDoc    = "docs/api/error-codes.md"

	billingUIDir = "sdk/billing-ui/src/client/generated/"
	consoleDir   = "web/admin/src/lib/api/generated/"
)

// browserGroups are the routes a browser SDK calls: billing-ui's wire types
// cover these, the console's cover every group.
var browserGroups = map[routes.Group]bool{routes.Checkout: true, routes.Customer: true, routes.Meta: true}

// Files renders every generated file from the route catalog and the
// error-code registry, by repository path. fsys is the repository: enum
// values are read from the source that declares them.
func Files(fsys fs.FS) (map[string][]byte, error) {
	all, err := newModel(fsys, routes.Catalog())
	if err != nil {
		return nil, err
	}
	var browser []routes.Route
	for _, r := range routes.Catalog() {
		if browserGroups[r.Group] {
			browser = append(browser, r)
		}
	}
	sdk, err := newModel(fsys, browser)
	if err != nil {
		return nil, err
	}
	openapi, err := all.openAPI()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		OpenAPIFile:                     openapi,
		RoutesDoc:                       all.routesMD(),
		CodesDoc:                        errorCodesMD(),
		billingUIDir + "wire.ts":        sdk.wireTS(),
		billingUIDir + "routes.ts":      sdk.routesTS(),
		billingUIDir + "error-codes.ts": errorCodesTS(),
		billingUIDir + "currencies.ts":  currenciesTS(),
		consoleDir + "wire.ts":          all.wireTS(),
		consoleDir + "routes.ts":        all.routesTS(),
		consoleDir + "error-codes.ts":   errorCodesTS(),
		consoleDir + "currencies.ts":    currenciesTS(),
	}, nil
}

// ErrStale is a committed generated file that no longer matches the catalog.
var ErrStale = errors.New("generated contract files are stale")

// Verify fails when a generated file in fsys differs from what the catalog
// renders, and names each one.
func Verify(fsys fs.FS) error {
	files, err := Files(fsys)
	if err != nil {
		return err
	}
	var stale []string
	for name, want := range files {
		if got, err := fs.ReadFile(fsys, name); err != nil || !bytes.Equal(got, want) {
			stale = append(stale, name)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Strings(stale)
	return fmt.Errorf("%w; run `go run ./scripts/contracts -write` and review the change:\n  %s", ErrStale, strings.Join(stale, "\n  "))
}
