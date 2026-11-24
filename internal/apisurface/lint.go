package apisurface

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// BaselineFile lists the findings the API still has, one "rule identifier"
// line each. It only shrinks: a new finding fails Lint, and so does a line no
// finding matches.
const BaselineFile = "api/lint-baseline.txt"

// rules: the identifier each finding starts with, and what it asks for.
var rules = map[string]string{
	"alias":          "1: a public package declares no type alias",
	"internal":       "1: no exported name has a type from an internal package",
	"reexport":       "1: no exported value is taken from an internal package",
	"untyped-const":  "2: an exported string constant has a type",
	"var":            "3: an exported variable is an Err* error",
	"enum-field":     "4: a string field named after an enum has the enum's type",
	"currency-field": "4: a currency field is a catalog.Currency",
	"ctx-first":      "5: a context.Context parameter comes first",
	"verb":           "5: a *Client method taking a context starts with a verb of verbs",
	"string-param":   "5: a positional string of a *Client method is a key, reference, requestID(s) or idempotencyKey",
	"list-page":      "6: a *Client List method returns a *billing.ListPage",
	"json-dash":      "7: no exported field is tagged json:\"-\"",
	"error-code":     "8: each billing.ErrorCodes() entry has a billing.ErrorCode constant",
}

// verbs is the closed list a *Client method name starts with.
var verbs = []string{
	"Get", "List", "Create", "Update", "Delete", "Set", "Ensure", "Verify",
	"Adjust", "Admit", "Apply", "Approve", "Cancel", "Capture", "Change", "Check",
	"Clear", "Dismiss", "Export", "Extend", "Import", "Link", "Mark", "Preview",
	"Query", "Record", "Refund", "Reinstate", "Release", "Resolve", "Resume", "Revoke",
	"Start", "Stop", "Sync", "Waive",
}

// stringParams are the names a *Client method's positional string may have.
var stringParams = []string{"key", "reference", "requestID", "requestIDs", "idempotencyKey"}

// An exception allows the findings of rule whose identifier matches pattern
// (path.Match). One that allows nothing fails Lint.
type exception struct {
	rule, pattern, reason string
}

// exceptions are the findings the conventions allow.
var exceptions = []exception{
	{"untyped-const", "openrails.Queue*", "River queue names, which River's config takes as strings"},
	{"untyped-const", "billing.Scope*", "OAuth scope strings of an issuer's access tokens"},
	{"untyped-const", "billing.*GroupPersona", "AuthKit permission-group personas, which AuthKit takes as strings"},
	{"untyped-const", "catalog.AccessForever", "a value of the catalog document's access duration string"},
	{"untyped-const", "catalog.BalanceLimit", "the built-in limit key; limit keys are strings"},
	{"untyped-const", "server.ConsoleClientID", "an OAuth client_id, an identifier like the host-chosen keys"},
	{"untyped-const", "billing.CardVaultPlatform", "a value of settings.card_vault, whose other values are PSP keys"},
	{"untyped-const", "adapters/fiber.RouteNamePrefix", "a prefix of Fiber's string route names"},
	{"untyped-const", "openrailstest/nmimock.InsufficientFunds", "NMI's response code, a string on NMI's wire"},
	{"enum-field", "openrails.LLMConfig.Model", "an LLM's model name, not a catalog.Model"},
	{"enum-field", "billing.ChangeEvidence.Read", "how a provider record was read, not an openrails.Read"},
	{"enum-field", "openrailstest/stripemock.*", "the mock serves Stripe's own objects and vocabulary"},
	{"currency-field", "openrailstest/stripemock.*", "the mock serves Stripe's own objects, whose currencies are lowercase"},
	{"list-page", "openrails.Client.ListEvents", "the shared eventlog protocol's page, not a cursor list"},
}

// Lint returns the surface's findings, sorted "rule identifier" lines, less
// those exceptions allow. An exception that allows nothing is an error.
func (s *Surface) Lint() ([]string, error) {
	l := &linter{found: map[string]bool{}}
	var enums []string
	for _, p := range s.pkgs {
		if err := l.pkg(p, s.pkgNames); err != nil {
			return nil, err
		}
		enums = append(enums, stringEnums(p.Types)...)
	}
	for _, id := range s.internal {
		l.add("internal", id)
	}
	for _, f := range s.fields {
		l.field(f, enums)
	}
	for _, p := range s.pkgs {
		switch p.Name {
		case "openrails":
			if obj, ok := p.Types.Scope().Lookup("Client").(*types.TypeName); ok {
				l.client(obj)
			}
		case "billing":
			l.errorCodes(p.Types)
		}
	}
	var findings, unused []string
	used := make([]bool, len(exceptions))
	for f := range l.found {
		rule, id, _ := strings.Cut(f, " ")
		allowed := false
		for i, e := range exceptions {
			if ok, _ := path.Match(e.pattern, id); ok && e.rule == rule {
				used[i], allowed = true, true
			}
		}
		if !allowed {
			findings = append(findings, f)
		}
	}
	for i, e := range exceptions {
		if !used[i] {
			unused = append(unused, e.rule+" "+e.pattern+" ("+e.reason+")")
		}
	}
	if len(unused) > 0 {
		return nil, fmt.Errorf("apisurface: exceptions that allow nothing; delete them:\n  %s", strings.Join(unused, "\n  "))
	}
	slices.Sort(findings)
	return findings, nil
}

// Compare checks findings against a baseline. It returns the baseline lines a
// finding still matches, and an error naming each finding the baseline lacks
// and each line that matches nothing.
func Compare(findings []string, baseline []byte) ([]string, error) {
	listed := map[string]bool{}
	var errs []error
	for line := range strings.Lines(string(baseline)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if listed[line] {
			errs = append(errs, fmt.Errorf("%s lists %q twice", BaselineFile, line))
		}
		listed[line] = true
	}
	var kept, added, stale []string
	for _, f := range findings {
		if listed[f] {
			kept = append(kept, f)
			delete(listed, f)
		} else {
			rule, _, _ := strings.Cut(f, " ")
			added = append(added, f+"\t(rule "+rules[rule]+")")
		}
	}
	for line := range listed {
		stale = append(stale, line)
	}
	slices.Sort(stale)
	if len(added) > 0 {
		errs = append(errs, fmt.Errorf("%d new API findings; fix them (%s only shrinks):\n  %s", len(added), BaselineFile, strings.Join(added, "\n  ")))
	}
	if len(stale) > 0 {
		errs = append(errs, fmt.Errorf("%d %s lines match nothing; drop them with go run ./scripts/contracts -lint -prune:\n  %s", len(stale), BaselineFile, strings.Join(stale, "\n  ")))
	}
	return kept, errors.Join(errs...)
}

// Baseline is the file holding lines.
func Baseline(lines []string) []byte {
	var b strings.Builder
	b.WriteString("# Go API findings still to fix: rule identifier. Lines are only removed\n# (go run ./scripts/contracts -lint -prune); internal/apisurface/lint.go has the rules.\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	return []byte(b.String())
}

type linter struct {
	found map[string]bool
}

func (l *linter) add(rule, id string) {
	l.found[rule+" "+id] = true
}

// pkg checks p's own declarations.
func (l *linter) pkg(p Package, pkgNames map[string]string) error {
	scope := p.Types.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		id := p.Name + "." + name
		switch obj := obj.(type) {
		case *types.TypeName:
			if obj.IsAlias() {
				l.add("alias", id)
			} else if n, ok := obj.Type().(*types.Named); ok {
				for m := range n.Methods() {
					if m.Exported() {
						l.ctxFirst(id+"."+m.Name(), m.Signature())
					}
				}
			}
		case *types.Const:
			if obj.Type() == types.Typ[types.UntypedString] {
				l.add("untyped-const", id)
			}
		case *types.Var:
			if !strings.HasPrefix(name, "Err") || !types.Identical(obj.Type(), types.Universe.Lookup("error").Type()) {
				l.add("var", id)
			}
		case *types.Func:
			l.ctxFirst(id, obj.Signature())
		}
	}
	return l.reexports(p, pkgNames)
}

// reexports finds exported constants and variables whose value is an internal
// package's: const X = internalpkg.Y.
func (l *linter) reexports(p Package, pkgNames map[string]string) error {
	fset := token.NewFileSet()
	for _, file := range p.Files {
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		internal := map[string]bool{}
		for _, spec := range f.Imports {
			ip, _ := strconv.Unquote(spec.Path.Value)
			name := pkgNames[ip]
			if spec.Name != nil {
				name = spec.Name.Name
			} else if name == "" {
				name = path.Base(ip)
			}
			internal[name] = internalPath(ip)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !name.IsExported() || i >= len(vs.Values) {
						continue
					}
					if sel, ok := ast.Unparen(vs.Values[i]).(*ast.SelectorExpr); ok {
						if x, ok := sel.X.(*ast.Ident); ok && internal[x.Name] {
							l.add("reexport", p.Name+"."+name.Name)
						}
					}
				}
			}
		}
	}
	return nil
}

func (l *linter) ctxFirst(id string, sig *types.Signature) {
	for i := range sig.Params().Len() {
		if i > 0 && isContext(sig.Params().At(i).Type()) {
			l.add("ctx-first", id)
		}
	}
}

var leadingWord = regexp.MustCompile(`^[A-Z][a-z]*`)

// client checks the methods of the *Client.
func (l *linter) client(obj *types.TypeName) {
	for sel := range types.NewMethodSet(types.NewPointer(obj.Type())).Methods() {
		m := sel.Obj().(*types.Func)
		sig := m.Signature()
		if !m.Exported() || sig.Params().Len() == 0 || !isContext(sig.Params().At(0).Type()) {
			continue
		}
		id := "openrails.Client." + m.Name()
		if !slices.Contains(verbs, leadingWord.FindString(m.Name())) {
			l.add("verb", id)
		}
		for i := 1; i < sig.Params().Len(); i++ {
			p := sig.Params().At(i)
			t := p.Type()
			if s, ok := t.(*types.Slice); ok && !(sig.Variadic() && i == sig.Params().Len()-1) {
				t = s.Elem()
			}
			if t == types.Typ[types.String] && !slices.Contains(stringParams, p.Name()) {
				name := p.Name()
				if name == "" || name == "_" {
					name = strconv.Itoa(i)
				}
				l.add("string-param", id+"."+name)
			}
		}
		if strings.HasPrefix(m.Name(), "List") && !listPage(sig.Results()) {
			l.add("list-page", id)
		}
	}
}

// listPage reports results (*billing.ListPage[T], error).
func listPage(r *types.Tuple) bool {
	if r.Len() != 2 || !types.Identical(r.At(1).Type(), types.Universe.Lookup("error").Type()) {
		return false
	}
	p, ok := r.At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	n, ok := p.Elem().(*types.Named)
	return ok && n.Obj().Name() == "ListPage" && n.Obj().Pkg().Path() == importPath("billing")
}

func (l *linter) field(f field, enums []string) {
	id := f.owner + "." + f.name
	if reflect.StructTag(f.tag).Get("json") == "-" {
		l.add("json-dash", id)
	}
	t := f.typ
	switch u := t.(type) {
	case *types.Pointer:
		t = u.Elem()
	case *types.Slice:
		t = u.Elem()
	}
	if t != types.Typ[types.String] {
		return
	}
	switch {
	case strings.HasSuffix(f.name, "Currency"):
		l.add("currency-field", id)
	default:
		for _, e := range enums {
			if strings.HasSuffix(f.name, e) {
				l.add("enum-field", id)
				break
			}
		}
	}
}

// errorCodes requires a billing.ErrorCode constant for each registered code.
func (l *linter) errorCodes(pkg *types.Package) {
	typed := map[string]bool{}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok || !c.Exported() || c.Val().Kind() != constant.String {
			continue
		}
		if n, ok := c.Type().(*types.Named); ok && n.Obj().Name() == "ErrorCode" && n.Obj().Pkg() == pkg {
			typed[constant.StringVal(c.Val())] = true
		}
	}
	for _, code := range billing.ErrorCodes() {
		if !typed[string(code.Code)] {
			l.add("error-code", string(code.Code))
		}
	}
}

// stringEnums are pkg's string types with constants.
func stringEnums(pkg *types.Package) []string {
	var enums []string
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		c, ok := scope.Lookup(name).(*types.Const)
		if !ok || !c.Exported() {
			continue
		}
		if n, ok := c.Type().(*types.Named); ok && n.Obj().Pkg() == pkg && n.Underlying() == types.Typ[types.String] && !slices.Contains(enums, n.Obj().Name()) {
			enums = append(enums, n.Obj().Name())
		}
	}
	return enums
}

func isContext(t types.Type) bool {
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Name() == "Context" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "context"
}
