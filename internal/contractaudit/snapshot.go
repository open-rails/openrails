// Package contractaudit captures the reviewed pre-v1 release contract and
// qualifies the release workflow manifest.
package contractaudit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const (
	Module       = "github.com/open-rails/openrails"
	SnapshotPath = "compatibility/contract.json"
)

// Snapshot is the reviewed contract. Every section is derived from source, so
// there is no second handwritten catalog to keep in sync.
type Snapshot struct {
	// API holds exported declarations of every importable non-main package,
	// plus internal type declarations reachable through that public surface.
	API map[string][]string `json:"go_api"`
	// Wire holds JSON-tagged struct shapes of non-public packages and the full
	// source of custom JSON/text codecs in any package.
	Wire map[string][]string `json:"wire_types"`
	// Imports resolves package selectors used by API and Wire declarations.
	Imports map[string]map[string]string `json:"declaring_file_imports"`
	// Sources fingerprints implementation that defines routes, authority,
	// status/error mapping, schema and canonical wire fixtures. Go sources are
	// fingerprinted without comments so documentation edits do not drift.
	Sources map[string]string `json:"boundary_sources_sha256"`
}

// Boundary implementation prefixes. Any other production file that imports the
// authority vocabulary is covered too, so new authorization code cannot opt out.
var (
	boundaryPrefixes = []string{
		"internal/auth/",
		"internal/controlplane/",
		"internal/http/",
		"internal/requestauth/",
		"internal/migrate/postgres/",
		"permissions/",
		"pkg/api/",
		"pkg/billingauth/",
		"testdata/wire/",
	}
	boundaryFiles    = map[string]bool{"errors.go": true}
	authorityImports = map[string]bool{Module + "/permissions": true, Module + "/pkg/billingauth": true, Module + "/internal/auth/policy": true}
	nonPublicRoots   = map[string]bool{"cmd": true, "internal": true, "scripts": true, "tests": true, "tools": true}
	wireCodecMethods = map[string]bool{"MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true, "UnmarshalText": true}
	ErrContractDrift = errors.New("reviewed release contract drifted")
)

type fileFacts struct {
	pkg     string
	api     []string
	wire    []string
	imports map[string]string
	source  string
	types   map[string][]reachableDeclaration
	roots   []typeReference
}

// capturer caches facts by path and content so one process can capture several
// trees without reparsing unchanged files.
type capturer struct {
	cache map[string]fileFacts
}

func newCapturer() *capturer { return &capturer{cache: map[string]fileFacts{}} }

// Capture reads the module rooted at fsys. Callers pass os.Root.FS() so every
// read stays inside the repository root.
func Capture(fsys fs.FS) ([]byte, error) { return newCapturer().capture(fsys) }

func (c *capturer) capture(fsys fs.FS) ([]byte, error) {
	out := Snapshot{API: map[string][]string{}, Wire: map[string][]string{}, Imports: map[string]map[string]string{}, Sources: map[string]string{}}
	files := map[string]fileFacts{}
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return walkDir(fsys, name, entry.Name())
		}
		if !covered(name) {
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		facts, err := c.facts(name, body)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(facts.api) > 0 {
			out.API[facts.pkg] = append(out.API[facts.pkg], facts.api...)
		}
		if len(facts.wire) > 0 {
			out.Wire[facts.pkg] = append(out.Wire[facts.pkg], facts.wire...)
		}
		if (len(facts.api) > 0 || len(facts.wire) > 0) && len(facts.imports) > 0 {
			out.Imports[name] = facts.imports
		}
		if facts.source != "" {
			out.Sources[name] = facts.source
		}
		files[name] = facts
		return nil
	})
	if err != nil {
		return nil, err
	}
	captureReachableTypes(&out, files)
	for _, section := range []map[string][]string{out.API, out.Wire} {
		for _, declarations := range section {
			sort.Strings(declarations)
		}
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Verify fails when the tree no longer matches its committed snapshot and
// names the reviewed entries that changed.
func Verify(fsys fs.FS) error { return newCapturer().verify(fsys) }

func (c *capturer) verify(fsys fs.FS) error {
	actual, err := c.capture(fsys)
	if err != nil {
		return err
	}
	expected, err := fs.ReadFile(fsys, SnapshotPath)
	if err != nil {
		return err
	}
	if bytes.Equal(actual, expected) {
		return nil
	}
	return fmt.Errorf("%w; review the change and run `go run ./scripts/contracts -write`:\n%s", ErrContractDrift, describeDrift(expected, actual))
}

func walkDir(fsys fs.FS, name, base string) error {
	if name == "." {
		return nil
	}
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "node_modules" || base == "vendor" || (base == "testdata" && name != "testdata" && name != "testdata/wire") {
		return fs.SkipDir
	}
	if _, err := fs.Stat(fsys, path.Join(name, "go.mod")); err == nil {
		return fs.SkipDir
	}
	return nil
}

func covered(name string) bool {
	switch path.Ext(name) {
	case ".go":
		return !strings.HasSuffix(name, "_test.go")
	case ".sql":
		return strings.HasPrefix(name, "internal/migrate/postgres/")
	case ".json":
		return strings.HasPrefix(name, "testdata/wire/")
	}
	return false
}

func (c *capturer) facts(name string, body []byte) (fileFacts, error) {
	key := name + "\x00" + digest(body)
	if facts, ok := c.cache[key]; ok {
		return facts, nil
	}
	facts, err := computeFacts(name, body)
	if err == nil {
		c.cache[key] = facts
	}
	return facts, err
}

func computeFacts(name string, body []byte) (fileFacts, error) {
	facts := fileFacts{pkg: path.Dir(name)}
	if path.Ext(name) != ".go" {
		facts.source = digest(body)
		return facts, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
	if err != nil {
		return facts, err
	}
	imports := map[string]string{}
	production := true
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return facts, err
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		imports[local] = importPath
		production = production && importPath != "testing"
	}
	if boundary(name, imports, production) {
		normalized, err := render(fset, file)
		if err != nil {
			return facts, err
		}
		facts.source = digest(append(directives(body), normalized...))
	}
	public := publicPackage(name, file.Name.Name)
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && wireCodecMethods[fn.Name.Name] {
			codec, err := render(fset, fn)
			if err != nil {
				return facts, err
			}
			facts.wire = append(facts.wire, codec)
		}
	}
	for _, decl := range file.Decls {
		declarations, err := declarationsOf(fset, decl, public)
		if err != nil {
			return facts, err
		}
		if public {
			facts.api = append(facts.api, declarations...)
		} else {
			facts.wire = append(facts.wire, declarations...)
		}
	}
	facts.imports = imports
	facts.types, facts.roots, err = declaredTypes(fset, file, facts.pkg, imports, public)
	if err != nil {
		return facts, err
	}
	return facts, nil
}

func boundary(name string, imports map[string]string, production bool) bool {
	if boundaryFiles[name] {
		return true
	}
	for _, prefix := range boundaryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if !production {
		return false
	}
	for _, importPath := range imports {
		if authorityImports[importPath] {
			return true
		}
	}
	return false
}

func publicPackage(name, pkg string) bool {
	if pkg == "main" {
		return false
	}
	segments := strings.Split(path.Dir(name), "/")
	if nonPublicRoots[segments[0]] {
		return false
	}
	for _, segment := range segments {
		if segment == "internal" {
			return false
		}
	}
	return true
}

// declarationsOf renders exported declarations for public packages. For other
// packages it renders only JSON-tagged struct types: their serialized shape is
// observable even when the Go type is not importable.
func declarationsOf(fset *token.FileSet, decl ast.Decl, public bool) ([]string, error) {
	var out []string
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if !public || !ast.IsExported(d.Name.Name) || (d.Recv != nil && !exportedReceiver(d.Recv.List[0].Type)) {
			return nil, nil
		}
		d.Body = nil
		rendered, err := render(fset, d)
		return append(out, rendered), err
	case *ast.GenDecl:
		if d.Tok == token.IMPORT {
			return nil, nil
		}
		if d.Tok == token.CONST && public {
			for _, spec := range d.Specs {
				for _, name := range spec.(*ast.ValueSpec).Names {
					if ast.IsExported(name.Name) {
						rendered, err := render(fset, d)
						return append(out, rendered), err
					}
				}
			}
			return nil, nil
		}
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				if (public && !ast.IsExported(s.Name.Name)) || (!public && !jsonTagged(s.Type)) {
					continue
				}
				dropUnexportedFields(s.Type)
				rendered, err := render(fset, s)
				if err != nil {
					return nil, err
				}
				out = append(out, "type "+rendered)
			case *ast.ValueSpec:
				if !public {
					continue
				}
				for _, name := range s.Names {
					if ast.IsExported(name.Name) {
						rendered, err := render(fset, s)
						if err != nil {
							return nil, err
						}
						out = append(out, d.Tok.String()+" "+rendered)
						break
					}
				}
			}
		}
	}
	return out, nil
}

func jsonTagged(expr ast.Expr) bool {
	shape, ok := expr.(*ast.StructType)
	if !ok {
		return false
	}
	tagged := false
	ast.Inspect(shape, func(node ast.Node) bool {
		if field, ok := node.(*ast.Field); ok && field.Tag != nil {
			if tag, err := strconv.Unquote(field.Tag.Value); err == nil {
				if _, ok := reflect.StructTag(tag).Lookup("json"); ok {
					tagged = true
				}
			}
		}
		return !tagged
	})
	return tagged
}

func dropUnexportedFields(expr ast.Expr) {
	shape, ok := expr.(*ast.StructType)
	if !ok {
		return
	}
	fields := shape.Fields.List[:0]
	for _, field := range shape.Fields.List {
		names := field.Names[:0]
		for _, name := range field.Names {
			if ast.IsExported(name.Name) {
				names = append(names, name)
			}
		}
		if len(field.Names) == 0 || len(names) > 0 {
			field.Names = names
			fields = append(fields, field)
		}
	}
	shape.Fields.List = fields
}

func exportedReceiver(t ast.Expr) bool {
	switch t := t.(type) {
	case *ast.Ident:
		return ast.IsExported(t.Name)
	case *ast.StarExpr:
		return exportedReceiver(t.X)
	case *ast.IndexExpr:
		return exportedReceiver(t.X)
	case *ast.IndexListExpr:
		return exportedReceiver(t.X)
	default:
		return false
	}
}

// directives keeps compiler directives, which change behavior despite being
// comments, in the comment-free source fingerprint.
func directives(body []byte) []byte {
	var out []byte
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), len(body)+1)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); strings.HasPrefix(line, "//go:") {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return out
}

func render(fset *token.FileSet, node any) (string, error) {
	var out bytes.Buffer
	if err := format.Node(&out, fset, node); err != nil {
		return "", err
	}
	return out.String(), nil
}

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func describeDrift(expectedRaw, actualRaw []byte) string {
	var expected, actual Snapshot
	if json.Unmarshal(expectedRaw, &expected) != nil || json.Unmarshal(actualRaw, &actual) != nil {
		return "  snapshot is not a valid contract document"
	}
	var lines []string
	for _, section := range []struct {
		name             string
		expected, actual map[string][]string
	}{{"go_api", expected.API, actual.API}, {"wire_types", expected.Wire, actual.Wire}} {
		for _, pkg := range keys(section.expected, section.actual) {
			removed, added := difference(section.expected[pkg], section.actual[pkg]), difference(section.actual[pkg], section.expected[pkg])
			for _, head := range keys(removed, added) {
				change := "changed"
				if !added[head] {
					change = "removed"
				} else if !removed[head] {
					change = "added"
				}
				lines = append(lines, fmt.Sprintf("  %s %s %s: %s", section.name, pkg, change, head))
			}
		}
	}
	for _, name := range keys(expected.Imports, actual.Imports) {
		if !reflect.DeepEqual(expected.Imports[name], actual.Imports[name]) {
			lines = append(lines, "  declaring_file_imports changed: "+name)
		}
	}
	for _, name := range keys(expected.Sources, actual.Sources) {
		if expected.Sources[name] != actual.Sources[name] {
			lines = append(lines, "  boundary source changed: "+name)
		}
	}
	if len(lines) == 0 {
		return "  snapshot formatting differs"
	}
	return strings.Join(lines, "\n")
}

func keys[V any](maps ...map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for key := range m {
			if !seen[key] {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	sort.Strings(out)
	return out
}

// difference returns the first lines of declarations in a that are not in b.
func difference(a, b []string) map[string]bool {
	present := make(map[string]bool, len(b))
	for _, declaration := range b {
		present[declaration] = true
	}
	out := map[string]bool{}
	for _, declaration := range a {
		if !present[declaration] {
			out[heading(declaration)] = true
		}
	}
	return out
}

// heading names a declaration in drift reports; grouped const/var blocks are
// named by their first member.
func heading(declaration string) string {
	lines := strings.Split(declaration, "\n")
	if len(lines) > 1 && strings.HasSuffix(lines[0], "(") {
		return lines[0] + " " + strings.TrimSpace(lines[1]) + " ..."
	}
	return lines[0]
}
