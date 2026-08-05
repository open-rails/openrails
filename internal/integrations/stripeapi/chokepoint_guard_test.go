package stripeapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// IDEM-8's audit method used to be:
//
//	grep -rn "stripe.com" --include=*.go | grep -v stripeapi
//
// which can NEVER fail meaningfully. OpenRails has no stripe-go dependency, so
// every Stripe call site — compliant or not — contains the literal URL, and the
// filter drops only this package. It returned 20+ "hits" on a fully compliant
// tree, so nobody could ever read the output as pass/fail.
//
// What actually matters is structural: a file that talks to Stripe must obtain
// its transport from this package, because the readonly guard and the
// Stripe-Version pin live in that transport and nowhere else. That is what these
// tests check, and unlike the grep they have an expected value: zero.

const stripeHost = "api.stripe.com"

func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}
	return root
}

type stripeFile struct {
	rel  string
	file *ast.File
	src  string
}

// stripeFiles returns every non-test Go file in the module that names the
// Stripe API host, excluding this package (which IS the choke point).
func stripeFiles(t *testing.T) []stripeFile {
	t.Helper()
	root := moduleRoot(t)
	var out []stripeFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch {
			case strings.HasPrefix(d.Name(), "."), d.Name() == "node_modules", d.Name() == "vendor":
				return filepath.SkipDir
			case rel == "internal/integrations/stripeapi":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(raw)
		if !strings.Contains(src, stripeHost) {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		out = append(out, stripeFile{rel: rel, file: f, src: src})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestStripeCallSitesUseTheChokePoint: any file naming api.stripe.com must
// import this package. Without it there is no way to have gone through the
// guarded transport, so readonly is unenforced and Stripe-Version unpinned.
func TestStripeCallSitesUseTheChokePoint(t *testing.T) {
	files := stripeFiles(t)
	if len(files) < 5 {
		t.Fatalf("only %d Stripe call sites found: the walk is broken and this guard would pass vacuously", len(files))
	}

	var violations []string
	for _, f := range files {
		imported := false
		for _, imp := range f.file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/open-rails/openrails/internal/integrations/stripeapi" {
				imported = true
				break
			}
		}
		if !imported {
			violations = append(violations, f.rel)
		}
	}
	if len(violations) > 0 {
		t.Fatalf("these files address %s but do not import the stripeapi choke point, so their requests miss the "+
			"readonly guard AND the Stripe-Version pin (IDEM-8):\n  %s\n"+
			"Obtain the client from stripeapi.Client / stripeapi.ReadOnlyClient.",
			stripeHost, strings.Join(violations, "\n  "))
	}
}

// TestStripeCallSitesBuildNoOwnTransport: importing the package is not enough
// if the file ALSO hands Stripe an unguarded client. Nothing outside stripeapi
// may pair a Stripe URL with http.DefaultClient, a composite &http.Client{...},
// or http.Get/Post/PostForm (which use http.DefaultClient implicitly).
func TestStripeCallSitesBuildNoOwnTransport(t *testing.T) {
	var violations []string
	for _, f := range stripeFiles(t) {
		ast.Inspect(f.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				pkg, ok := node.X.(*ast.Ident)
				if !ok || pkg.Name != "http" {
					return true
				}
				switch node.Sel.Name {
				case "DefaultClient", "DefaultTransport", "Get", "Post", "PostForm", "Head":
					violations = append(violations, fmt.Sprintf("%s: http.%s", f.rel, node.Sel.Name))
				}
			case *ast.CompositeLit:
				sel, ok := node.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if ok && pkg.Name == "http" && sel.Sel.Name == "Client" {
					violations = append(violations, f.rel+": &http.Client{...}")
				}
			}
			return true
		})
	}
	if len(violations) > 0 {
		t.Fatalf("these Stripe call sites build their own transport, bypassing the readonly guard and the "+
			"Stripe-Version pin (IDEM-8):\n  %s", strings.Join(violations, "\n  "))
	}
}

// or#865 residual: Client(nil, …) used to return a WRITE-capable client — the
// one input carrying no information about the operating mode produced the most
// permissive result. It now fails closed.
func TestNilConfigYieldsReadOnlyClient(t *testing.T) {
	c := Client(nil, 0)
	tr, ok := c.Transport.(*guardTransport)
	if !ok {
		t.Fatalf("client transport is %T, not the guard", c.Transport)
	}
	if !tr.readOnly {
		t.Fatal("Client(nil, …) must fail closed: an unknown operating mode may not authorize provider writes")
	}
}
