// Package apisurface lists the Go API that OpenRails v1 freezes: every exported
// constant, variable, function, type, field (with its struct tag) and method of
// the public packages, one per line, in api/go.txt; and lints it against the
// API conventions (Lint).
package apisurface

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// Module is OpenRails' module path.
	Module = "github.com/open-rails/openrails"
	// File is where the list lives, relative to the repository.
	File = "api/go.txt"
)

// Packages are the covered packages, relative to Module.
var Packages = []string{
	"", "adapters/authkit", "adapters/fiber", "adapters/gin", "adapters/http", "billing", "catalog", "openrailstest", "openrailstest/nmimock", "openrailstest/stripemock", "server", "web/console", "web/pay",
}

// ServerModule is the directory of the module released in lockstep with the
// root; its packages are listed from there.
const ServerModule = "server"

// AuthKitAdapterModule is the opt-in module that may link AuthKit runtime.
const AuthKitAdapterModule = "adapters/authkit"

// Surface is the covered API.
type Surface struct {
	// Features is one sorted line per exported feature.
	Features []string

	pkgs     []Package
	pkgNames map[string]string // import path to package name
	fields   []field
	internal []string // features whose type names an internal package's type
}

// Package is one covered package.
type Package struct {
	// Name is "openrails" for the root, else the path below Module.
	Name  string
	Types *types.Package
	// Files are its non-test Go files.
	Files []string
}

// field is an exported struct field: owner.name.
type field struct {
	owner, name string
	typ         types.Type
	tag         string
}

// Load type-checks the covered packages from their export data (go list
// -export) and lists their features. repo is the repository root; each module's
// packages are listed in that module.
func Load(ctx context.Context, repo string) (*Surface, error) {
	paths := make([]string, len(Packages))
	byModule := map[string][]string{}
	for i, rel := range Packages {
		paths[i] = importPath(rel)
		dir := "."
		if rel == ServerModule || strings.HasPrefix(rel, ServerModule+"/") {
			dir = ServerModule
		} else if rel == AuthKitAdapterModule {
			dir = AuthKitAdapterModule
		}
		byModule[dir] = append(byModule[dir], paths[i])
	}
	// One importer over the module lists: a root package keeps the root module's
	// export data, so both modules' packages share its types.
	listed := map[string]listing{}
	for _, dir := range []string{".", ServerModule, AuthKitAdapterModule} {
		if err := listExports(ctx, filepath.Join(repo, dir), byModule[dir], listed); err != nil {
			return nil, err
		}
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		if listed[path].export == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(listed[path].export)
	})
	pkgs := make([]Package, len(paths))
	for i, path := range paths {
		pkg, err := imp.Import(path)
		if err != nil {
			return nil, err
		}
		name := "openrails"
		if Packages[i] != "" {
			name = Packages[i]
		}
		pkgs[i] = Package{Name: name, Types: pkg, Files: listed[path].files}
	}
	names := map[string]string{}
	for path, l := range listed {
		names[path] = l.name
	}
	return surfaceOf(pkgs, names), nil
}

// surfaceOf lists the features of pkgs; names maps import paths to package
// names.
func surfaceOf(pkgs []Package, names map[string]string) *Surface {
	s := &surfacer{}
	for _, p := range pkgs {
		s.walk(p.Types, p.Name)
	}
	slices.Sort(s.lines)
	return &Surface{Features: slices.Compact(s.lines), pkgs: pkgs, pkgNames: names, fields: s.fields, internal: s.internal}
}

type listing struct {
	name, export string
	files        []string
}

func listExports(ctx context.Context, dir string, paths []string, listed map[string]listing) error {
	cmd := exec.CommandContext(ctx, "go", append([]string{"list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Name}}\t{{.Export}}\t{{.Dir}}\t{{join .GoFiles \"\\t\"}}"}, paths...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list in %s: %w: %s", dir, err, stderr.Bytes())
	}
	for line := range strings.Lines(string(out)) {
		parts := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if _, ok := listed[parts[0]]; ok || len(parts) < 4 {
			continue
		}
		l := listing{name: parts[1], export: parts[2]}
		for _, f := range parts[4:] {
			if f != "" {
				l.files = append(l.files, filepath.Join(parts[3], f))
			}
		}
		listed[parts[0]] = l
	}
	return nil
}

// Text is the list as api/go.txt holds it.
func (s *Surface) Text() []byte {
	return []byte(strings.Join(s.Features, "\n") + "\n")
}

// ErrStale reports a stale api/go.txt.
var ErrStale = errors.New(File + " is stale: run go run ./scripts/contracts -write")

func importPath(rel string) string {
	if rel == "" {
		return Module
	}
	return Module + "/" + rel
}

type surfacer struct {
	lines    []string
	fields   []field
	internal []string
}

// walk lists pkg's exported features; name is its Package.Name.
func (s *surfacer) walk(pkg *types.Package, name string) {
	listed := "openrails"
	if name != listed {
		listed += "/" + name
	}
	w := &writer{s: s, pkg: pkg, listed: listed, name: name}
	scope := pkg.Scope()
	for _, id := range scope.Names() {
		switch obj := scope.Lookup(id).(type) {
		case *types.Const:
			if obj.Exported() {
				w.emit(id, "const %s %s = %s", id, w.typ(obj.Type()), obj.Val().ExactString())
			}
		case *types.Var:
			if obj.Exported() {
				w.emit(id, "var %s %s", id, w.typ(obj.Type()))
			}
		case *types.Func:
			if obj.Exported() {
				w.emit(id, "func %s%s", id, w.sig(obj.Signature()))
			}
		case *types.TypeName:
			if obj.Exported() {
				w.typeName(obj)
			}
		}
	}
}

// internalPkg reports a package of either module under an internal/ directory.
func internalPkg(p *types.Package) bool {
	return p != nil && internalPath(p.Path())
}

func internalPath(path string) bool {
	return strings.HasPrefix(path, Module+"/") && slices.Contains(strings.Split(path, "/"), "internal")
}

type writer struct {
	s      *surfacer
	pkg    *types.Package
	listed string // the package's name in File
	name   string // the package's name in findings
	// hit: a type spelled since the last emit came from an internal package.
	hit bool
}

// emit lists one feature; id names it below the package.
func (w *writer) emit(id, format string, args ...any) {
	w.s.lines = append(w.s.lines, "pkg "+w.listed+", "+fmt.Sprintf(format, args...))
	if w.hit && id != "" {
		w.s.internal = append(w.s.internal, w.name+"."+id)
	}
	w.hit = false
}

func (w *writer) typeName(obj *types.TypeName) {
	name := obj.Name()
	if a, ok := obj.Type().(*types.Alias); ok {
		// The alias is the finding (rule 1), not the type it names.
		w.emit("", "type %s%s = %s", name, w.tparams(a.TypeParams()), w.typ(a.Rhs()))
		// An alias of an internal type is how the covered API spells it: list
		// the target's members under the alias's name.
		if n, ok := types.Unalias(a).(*types.Named); ok && internalPkg(n.Obj().Pkg()) {
			w.members(name, n.Origin())
		}
		return
	}
	if n, ok := obj.Type().(*types.Named); ok {
		w.members(name, n)
	}
}

func (w *writer) members(name string, n *types.Named) {
	tp := w.tparams(n.TypeParams())
	switch u := n.Underlying().(type) {
	case *types.Struct:
		w.emit(name, "type %s%s struct", name, tp)
		w.fields(name, u)
	case *types.Interface:
		w.emit(name, "type %s%s interface", name, tp)
		sealed := false
		for i := range u.NumMethods() {
			m := u.Method(i)
			if !m.Exported() {
				sealed = true
				continue
			}
			w.emit(name+"."+m.Name(), "type %s interface, %s%s", name, m.Name(), w.sig(m.Signature()))
		}
		for i := range u.NumEmbeddeds() {
			if _, ok := u.EmbeddedType(i).Underlying().(*types.Interface); !ok {
				t := w.typ(u.EmbeddedType(i))
				w.emit(name+"."+t, "type %s interface, %s", name, t)
			}
		}
		if sealed {
			w.emit("", "type %s interface, unexported methods", name)
		}
		return
	default:
		w.emit(name, "type %s%s %s", name, tp, w.typ(u))
	}
	recv := name
	if tp != "" {
		var names []string
		for t := range n.TypeParams().TypeParams() {
			names = append(names, t.Obj().Name())
		}
		recv += "[" + strings.Join(names, ", ") + "]"
	}
	value := types.NewMethodSet(n)
	ptr := types.NewMethodSet(types.NewPointer(n))
	for sel := range ptr.Methods() {
		m := sel.Obj()
		if !m.Exported() {
			continue
		}
		r := "*" + recv
		if value.Lookup(m.Pkg(), m.Name()) != nil {
			r = recv
		}
		w.emit(name+"."+m.Name(), "method (%s) %s%s", r, m.Name(), w.sig(sel.Type().(*types.Signature)))
	}
}

func (w *writer) fields(name string, st *types.Struct) {
	for i := range st.NumFields() {
		f := st.Field(i)
		if f.Embedded() {
			w.emit(name+"."+f.Name(), "type %s struct, embedded %s", name, w.typ(f.Type()))
			if !f.Exported() {
				// An unexported embedded struct still promotes its exported fields.
				if inner, ok := f.Type().Underlying().(*types.Struct); ok {
					w.fields(name, inner)
				}
			}
			continue
		}
		if !f.Exported() {
			continue
		}
		tag := ""
		if t := st.Tag(i); t != "" {
			tag = " `" + t + "`"
		}
		w.emit(name+"."+f.Name(), "type %s struct, %s %s%s", name, f.Name(), w.typ(f.Type()), tag)
		w.s.fields = append(w.s.fields, field{owner: w.name + "." + name, name: f.Name(), typ: f.Type(), tag: st.Tag(i)})
	}
}

func (w *writer) tparams(list *types.TypeParamList) string {
	if list.Len() == 0 {
		return ""
	}
	var parts []string
	for t := range list.TypeParams() {
		parts = append(parts, t.Obj().Name()+" "+w.typ(t.Constraint()))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// sig is s without parameter names, which are not API.
func (w *writer) sig(s *types.Signature) string {
	var b strings.Builder
	b.WriteString(w.tparams(s.TypeParams()))
	b.WriteString("(")
	for i := range s.Params().Len() {
		if i > 0 {
			b.WriteString(", ")
		}
		t := s.Params().At(i).Type()
		if s.Variadic() && i == s.Params().Len()-1 {
			b.WriteString("..." + w.typ(t.(*types.Slice).Elem()))
		} else {
			b.WriteString(w.typ(t))
		}
	}
	b.WriteString(")")
	switch r := s.Results(); r.Len() {
	case 0:
	case 1:
		b.WriteString(" " + w.typ(r.At(0).Type()))
	default:
		var parts []string
		for v := range r.Variables() {
			parts = append(parts, w.typ(v.Type()))
		}
		b.WriteString(" (" + strings.Join(parts, ", ") + ")")
	}
	return b.String()
}

func (w *writer) qualified(obj *types.TypeName, args *types.TypeList) string {
	s := obj.Name()
	if p := obj.Pkg(); p != nil && p != w.pkg {
		s = p.Name() + "." + s
	}
	if args.Len() > 0 {
		var parts []string
		for t := range args.Types() {
			parts = append(parts, w.typ(t))
		}
		s += "[" + strings.Join(parts, ", ") + "]"
	}
	return s
}

func (w *writer) typ(t types.Type) string {
	switch t := t.(type) {
	case *types.Basic:
		return t.Name()
	case *types.Pointer:
		return "*" + w.typ(t.Elem())
	case *types.Slice:
		return "[]" + w.typ(t.Elem())
	case *types.Array:
		return fmt.Sprintf("[%d]%s", t.Len(), w.typ(t.Elem()))
	case *types.Map:
		return "map[" + w.typ(t.Key()) + "]" + w.typ(t.Elem())
	case *types.Chan:
		switch t.Dir() {
		case types.SendOnly:
			return "chan<- " + w.typ(t.Elem())
		case types.RecvOnly:
			return "<-chan " + w.typ(t.Elem())
		}
		return "chan " + w.typ(t.Elem())
	case *types.Signature:
		return "func" + w.sig(t)
	case *types.Named:
		w.hit = w.hit || internalPkg(t.Obj().Pkg())
		return w.qualified(t.Obj(), t.TypeArgs())
	case *types.Alias:
		// An alias a covered package declares is its own finding.
		w.hit = w.hit || internalPkg(t.Obj().Pkg())
		return w.qualified(t.Obj(), t.TypeArgs())
	case *types.TypeParam:
		return t.Obj().Name()
	case *types.Union:
		var parts []string
		for term := range t.Terms() {
			p := w.typ(term.Type())
			if term.Tilde() {
				p = "~" + p
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, " | ")
	case *types.Interface:
		if t.IsImplicit() && t.NumEmbeddeds() == 1 {
			return w.typ(t.EmbeddedType(0))
		}
		var parts []string
		for i := range t.NumEmbeddeds() {
			parts = append(parts, w.typ(t.EmbeddedType(i)))
		}
		for i := range t.NumExplicitMethods() {
			m := t.ExplicitMethod(i)
			parts = append(parts, m.Name()+w.sig(m.Signature()))
		}
		if len(parts) == 0 {
			return "interface{}"
		}
		return "interface{ " + strings.Join(parts, "; ") + " }"
	case *types.Struct:
		var parts []string
		for i := range t.NumFields() {
			f := t.Field(i)
			p := w.typ(f.Type())
			if !f.Embedded() {
				p = f.Name() + " " + p
			}
			if tag := t.Tag(i); tag != "" {
				p += " `" + tag + "`"
			}
			parts = append(parts, p)
		}
		return "struct{ " + strings.Join(parts, "; ") + " }"
	}
	return t.String()
}
