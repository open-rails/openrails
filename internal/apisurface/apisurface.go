// Package apisurface lists the Go API that OpenRails v1 freezes, one feature
// per line: every exported constant, variable, function, type, field (with its
// struct tag) and method of the public packages. api/go.txt holds the list;
// go run ./scripts/contracts -write rewrites it and TestGoAPISurface fails when it is stale.
package apisurface

import (
	"bytes"
	"errors"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
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
	"", "adapters/fiber", "adapters/gin", "adapters/http", "billing", "catalog", "web/admin",
}

// Surface is the covered API.
type Surface struct {
	// Features is one sorted line per exported feature.
	Features []string
	// Leaks are internal types the covered API reaches without a covered
	// package naming them by an alias.
	Leaks []string
	// AliasMethods are the exported methods a covered alias of an internal
	// struct or scalar type brings with it.
	AliasMethods []string
}

// Load type-checks the covered packages from their export data (go list
// -export) and lists their features. It runs the go command in the working
// directory, which must be inside the module.
func Load() (*Surface, error) {
	paths := make([]string, len(Packages))
	for i, rel := range Packages {
		paths[i] = importPath(rel)
	}
	cmd := exec.Command("go", append([]string{"list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}, paths...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, stderr.Bytes())
	}
	exports := map[string]string{}
	for line := range strings.Lines(string(out)) {
		path, file, _ := strings.Cut(strings.TrimSpace(line), "\t")
		exports[path] = file
	}
	imp := importer.ForCompiler(token.NewFileSet(), "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})

	s := &surfacer{aliased: map[*types.TypeName]bool{}, reached: map[*types.TypeName]bool{}}
	for i, path := range paths {
		pkg, err := imp.Import(path)
		if err != nil {
			return nil, err
		}
		name := "openrails"
		if Packages[i] != "" {
			name += "/" + Packages[i]
		}
		s.walk(pkg, name)
	}
	slices.Sort(s.lines)
	surface := &Surface{Features: slices.Compact(s.lines)}
	for tn := range s.reached {
		if !s.aliased[tn] {
			surface.Leaks = append(surface.Leaks, tn.Pkg().Path()+"."+tn.Name())
		}
	}
	slices.Sort(surface.Leaks)
	slices.Sort(s.aliasMethods)
	surface.AliasMethods = s.aliasMethods
	return surface, nil
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
	lines   []string
	aliased map[*types.TypeName]bool // internal types a covered alias names
	reached map[*types.TypeName]bool // internal types the covered API reaches

	aliasMethods []string
}

// walk lists pkg's exported features under name.
func (s *surfacer) walk(pkg *types.Package, name string) {
	w := &writer{s: s, pkg: pkg, name: name}
	scope := pkg.Scope()
	for _, id := range scope.Names() {
		switch obj := scope.Lookup(id).(type) {
		case *types.Const:
			if obj.Exported() {
				w.emit("const %s %s = %s", id, w.typ(obj.Type()), obj.Val().ExactString())
			}
		case *types.Var:
			if obj.Exported() {
				w.emit("var %s %s", id, w.typ(obj.Type()))
			}
		case *types.Func:
			if obj.Exported() {
				w.emit("func %s%s", id, w.sig(obj.Signature()))
			}
		case *types.TypeName:
			if obj.Exported() {
				w.typeName(obj)
			}
		}
	}
}

func (s *surfacer) internal(p *types.Package) bool {
	return p != nil && strings.HasPrefix(p.Path(), Module+"/internal/")
}

type writer struct {
	s    *surfacer
	pkg  *types.Package
	name string
}

func (w *writer) emit(format string, args ...any) {
	w.s.lines = append(w.s.lines, "pkg "+w.name+", "+fmt.Sprintf(format, args...))
}

func (w *writer) typeName(obj *types.TypeName) {
	name := obj.Name()
	if a, ok := obj.Type().(*types.Alias); ok {
		w.emit("type %s%s = %s", name, w.tparams(a.TypeParams()), w.typ(a.Rhs()))
		// An alias of an internal type is how the covered API spells it: list
		// the target's members under the alias's name.
		if n, ok := types.Unalias(a).(*types.Named); ok && w.s.internal(n.Obj().Pkg()) {
			w.s.aliased[n.Origin().Obj()] = true
			before := len(w.s.lines)
			w.members(name, n.Origin())
			for _, line := range w.s.lines[before:] {
				if _, method, ok := strings.Cut(line, ", method "); ok {
					w.s.aliasMethods = append(w.s.aliasMethods, w.name+": "+method)
				}
			}
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
		w.emit("type %s%s struct", name, tp)
		w.fields(name, u)
	case *types.Interface:
		w.emit("type %s%s interface", name, tp)
		sealed := false
		for i := range u.NumMethods() {
			m := u.Method(i)
			if !m.Exported() {
				sealed = true
				continue
			}
			w.emit("type %s interface, %s%s", name, m.Name(), w.sig(m.Signature()))
		}
		for i := range u.NumEmbeddeds() {
			if _, ok := u.EmbeddedType(i).Underlying().(*types.Interface); !ok {
				w.emit("type %s interface, %s", name, w.typ(u.EmbeddedType(i)))
			}
		}
		if sealed {
			w.emit("type %s interface, unexported methods", name)
		}
		return
	default:
		w.emit("type %s%s %s", name, tp, w.typ(u))
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
		w.emit("method (%s) %s%s", r, m.Name(), w.sig(sel.Type().(*types.Signature)))
	}
}

func (w *writer) fields(name string, st *types.Struct) {
	for i := range st.NumFields() {
		f := st.Field(i)
		if f.Embedded() {
			w.emit("type %s struct, embedded %s", name, w.typ(f.Type()))
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
		w.emit("type %s struct, %s %s%s", name, f.Name(), w.typ(f.Type()), tag)
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
		if w.s.internal(t.Obj().Pkg()) {
			// Follow what an internal type exposes, so a leak behind it shows.
			w.reach(t.Origin())
		}
		return w.qualified(t.Obj(), t.TypeArgs())
	case *types.Alias:
		if n, ok := types.Unalias(t).(*types.Named); ok && w.s.internal(n.Obj().Pkg()) {
			w.reach(n.Origin())
		}
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

// reach records the internal types n's exported members expose.
func (w *writer) reach(n *types.Named) {
	if w.s.reached[n.Obj()] {
		return
	}
	w.s.reached[n.Obj()] = true
	probe := &writer{s: w.s, pkg: n.Obj().Pkg(), name: "-"}
	saved := len(w.s.lines)
	probe.members(n.Obj().Name(), n)
	w.s.lines = w.s.lines[:saved]
}
