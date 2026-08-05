package service

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// or#900: every exported *Service method that can reach the database must pin a
// merchant-scoped connection itself.
//
// This exists because the failure it prevents is SILENT. There is no privileged
// pool (or#868): under the or#885-mandated openrails_app role a read of a
// policied table on an unpinned handle matches `merchant_id = NULL` and returns
// ZERO ROWS AND NO ERROR. That is how PreviewPlanMigration shipped, passed its
// tests, and told a host "source price: no rows" about a price the host had just
// written — the facade half that pinned (the money surfaces, #227) and the half
// that relied on the caller were indistinguishable from the outside.
//
// The rule is deliberately syntactic and local: the method body must call
// s.pin(ctx) (the prologue) or open a scope itself (RunInMerchantConn /
// MerchantTx / RunInTx-inside-a-scope …). "The module service handles it" is
// exactly the belief that produced or#862, so cross-package delegation does not
// count — an exemption has to be written down here with its reason.
var facadeMethodsWithoutAPin = map[string]string{
	"GetCredits":         "returns an error unconditionally (currency is required; use GetCreditsByType) — touches nothing",
	"GetSupportedTokens": "reads the merchant's Solana rail CONFIG off the runtime, never a table",
	"HandleWebhook":      "the merchant is not known until the payload is authenticated; the webhook path pins after it resolves one",
	"ReleaseHold":        "frees a Redis reservation (#513); the durable ledger is untouched",
}

var pinners = []string{"pin", "RunInMerchantConn", "RunInMerchantScope", "MerchantTx", "WithMerchantConn"}

func TestEveryExportedFacadeMethodPinsAMerchantConnection(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	var unpinned []string
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			if facadeReceiver(fn.Recv.List[0].Type) != "Service" || !fn.Name.IsExported() {
				continue
			}
			seen[fn.Name.Name] = true
			if _, exempt := facadeMethodsWithoutAPin[fn.Name.Name]; exempt {
				continue
			}
			if !callsAPinner(fn.Body) {
				unpinned = append(unpinned, fn.Name.Name+" ("+name+")")
			}
		}
	}

	if len(unpinned) > 0 {
		sort.Strings(unpinned)
		t.Errorf(`or#900: these exported facade methods reach the database without pinning a merchant connection:

  %s

Under openrails_app an unpinned read of a policied table returns ZERO ROWS AND
NO ERROR — the call appears to succeed and answers nothing.

Fix it by opening the method with the prologue:

	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return …, pinErr
	}
	defer release()

or, if the method genuinely touches no table, add it to
facadeMethodsWithoutAPin WITH the reason.`, strings.Join(unpinned, "\n  "))
	}

	for m := range facadeMethodsWithoutAPin {
		if !seen[m] {
			t.Errorf("or#900: %q is exempted but is no longer an exported *Service method; delete the exemption", m)
		}
	}
}

func facadeReceiver(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return facadeReceiver(t.X)
	}
	return ""
}

func callsAPinner(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		case *ast.Ident:
			name = fun.Name
		}
		for _, p := range pinners {
			if name == p {
				found = true
			}
		}
		return true
	})
	return found
}
