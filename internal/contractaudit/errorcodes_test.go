package contractaudit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

// dynamicCodeFiles forward a code another site already named (a service
// refusal, a host hook's GateError). A new entry needs the same justification.
var dynamicCodeFiles = map[string]bool{
	"internal/billingauth/authenticator.go":         true,
	"internal/billingauth/gate.go":                  true,
	"internal/http/request/request.go":              true,
	"internal/http/handlers/refusal.go":             true,
	"internal/http/handlers/invoices.go":            true,
	"internal/http/handlers/admin_metering.go":      true,
	"internal/http/handlers/admissions.go":          true,
	"internal/http/handlers/subscription_change.go": true,
	"internal/http/handlers/orders.go":              true,
	"internal/http/handlers/provider_operations.go": true,
	"internal/http/router/merchant_selectors.go":    true,
	// The route gate and the test Auth refuse with the code their callers chose.
	"internal/http/routes/gate.go":              true,
	"internal/billingauth/authtest/authtest.go": true,
}

var httpStatus = map[string]int{
	"StatusBadRequest": 400, "StatusUnauthorized": 401, "StatusPaymentRequired": 402, "StatusForbidden": 403,
	"StatusNotFound": 404, "StatusMethodNotAllowed": 405, "StatusConflict": 409, "StatusGone": 410,
	"StatusRequestEntityTooLarge": 413, "StatusUnsupportedMediaType": 415, "StatusUnprocessableEntity": 422,
	"StatusTooManyRequests": 429, "StatusInternalServerError": 500, "StatusBadGateway": 502,
	"StatusServiceUnavailable": 503, "StatusGatewayTimeout": 504,
}

type sourceFile struct {
	name    string
	dir     string
	fset    *token.FileSet
	file    *ast.File
	imports map[string]string // local name -> module-relative directory
}

// constant is a package-level constant's value, read in its declaring file.
type constant struct {
	expr ast.Expr
	file sourceFile
}

// constants is every package-level constant in the tree, by directory.
type constants map[string]map[string]constant

func (c constants) resolve(f sourceFile, expr ast.Expr, depth int) (string, bool) {
	if depth > 4 {
		return "", false
	}
	switch x := expr.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(x.Value)
		return value, err == nil
	case *ast.Ident:
		if next, ok := c[f.dir][x.Name]; ok {
			return c.resolve(next.file, next.expr, depth+1)
		}
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		dir, ok := f.imports[pkg.Name]
		if !ok {
			return "", false
		}
		if next, ok := c[dir][x.Sel.Name]; ok {
			return c.resolve(next.file, next.expr, depth+1)
		}
	}
	return "", false
}

func statusOf(expr ast.Expr) (int, bool) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		n, err := strconv.Atoi(x.Value)
		return n, err == nil && x.Kind == token.INT
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" {
			n, ok := httpStatus[x.Sel.Name]
			return n, ok
		}
	}
	return 0, false
}

func loadSources(t *testing.T) ([]sourceFile, constants) {
	t.Helper()
	fsys := repositoryFS(t)
	var files []sourceFile
	consts := constants{}
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return walkDir(fsys, name, entry.Name())
		}
		if path.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		src := sourceFile{name: name, dir: path.Dir(name), fset: fset, file: file, imports: map[string]string{}}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			if !strings.HasPrefix(importPath, Module) {
				continue
			}
			dir := strings.TrimPrefix(strings.TrimPrefix(importPath, Module), "/")
			if dir == "" {
				dir = "."
			}
			local := path.Base(importPath)
			if spec.Name != nil {
				local = spec.Name.Name
			}
			src.imports[local] = dir
		}
		files = append(files, src)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i < len(vs.Values) {
						if consts[src.dir] == nil {
							consts[src.dir] = map[string]constant{}
						}
						consts[src.dir][id.Name] = constant{vs.Values[i], src}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, consts
}

// codeSite is one place a refusal names its code: the code, status and type
// argument expressions (nil when the constructor takes none).
type codeSite struct{ code, status, typ ast.Expr }

func siteOf(f sourceFile, call *ast.CallExpr) (codeSite, bool) {
	name, pkg := "", ""
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
		if id, ok := fn.X.(*ast.Ident); ok {
			pkg = id.Name
		}
	}
	arg := func(i int) ast.Expr {
		if i < len(call.Args) {
			return call.Args[i]
		}
		return nil
	}
	switch {
	case name == "NewAPIError" && len(call.Args) == 4:
		return codeSite{arg(2), arg(0), arg(1)}, true
	case name == "New" && len(call.Args) == 3 && (pkg == "apperr" || (pkg == "" && f.dir == "internal/shared/apperr")):
		return codeSite{arg(1), arg(0), nil}, true
	case name == "WriteJSONError" && len(call.Args) == 4:
		return codeSite{arg(2), arg(1), nil}, true
	case name == "newCodedError" && len(call.Args) == 2:
		return codeSite{arg(0), nil, nil}, true
	case (name == "Coded" || name == "ErrorCode" || name == "AbortCode") && len(call.Args) == 2:
		return codeSite{arg(0), nil, nil}, true
	case name == "Refusal" && len(call.Args) >= 1 && (pkg == "billingauth" || (pkg == "" && f.dir == "internal/billingauth")):
		return codeSite{arg(0), nil, nil}, true
	}
	return codeSite{}, false
}

// Every refusal the tree constructs names a registered code, under the status
// and type the registry gives it.
func TestRefusalsNameRegisteredCodes(t *testing.T) {
	files, consts := loadSources(t)
	checked := 0
	forwards := map[string]bool{}
	for _, f := range files {
		position := func(node ast.Node) string { return fmt.Sprintf("%s:%d", f.name, f.fset.Position(node.Pos()).Line) }
		ast.Inspect(f.file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			site, ok := siteOf(f, call)
			if !ok {
				return true
			}
			code, ok := consts.resolve(f, site.code, 0)
			if !ok {
				forwards[f.name] = true
				if !dynamicCodeFiles[f.name] {
					t.Errorf("%s: a refusal's code must be a registered constant or literal; forwarding files are listed in dynamicCodeFiles", position(call))
				}
				return true
			}
			info, registered := billing.LookupErrorCode(code)
			if !registered {
				t.Errorf("%s: error code %q is not registered in billing/error_codes.go", position(call), code)
				return true
			}
			checked++
			if status, ok := statusOf(site.status); ok && site.status != nil && status != info.Status {
				t.Errorf("%s: %s answers %d here; the registry says %d", position(call), code, status, info.Status)
			}
			if site.typ != nil {
				if typ, ok := consts.resolve(f, site.typ, 0); ok && typ != info.Type {
					t.Errorf("%s: %s is typed %s here; the registry says %s", position(call), code, typ, info.Type)
				}
			}
			return true
		})
	}
	for name := range dynamicCodeFiles {
		if !forwards[name] {
			t.Errorf("%s forwards no code any more; remove it from dynamicCodeFiles", name)
		}
	}
	if checked < 150 {
		t.Fatalf("only %d refusal sites were checked; the scan lost its targets", checked)
	}
}
