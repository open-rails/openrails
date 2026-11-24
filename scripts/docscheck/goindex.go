package main

import (
	"bufio"
	"go/ast"
	"go/token"
	"go/types"
	"regexp"
	"strings"
)

// goType is one exported type of api/go.txt: its fields (name to type),
// methods (type "") and embedded types.
type goType struct {
	pkg, name string
	members   map[string]string
	embedded  []string
	alias     string // the target of `type T = q.N`
}

// goIndex is api/go.txt: package-level names and types by short package
// name, and which names are only ever a method.
type goIndex struct {
	names  map[string]map[string]bool
	funcs  map[string]map[string]bool
	types  map[string]map[string]*goType
	byName map[string][]*goType
	all    map[string]bool
	other  map[string]bool // names that are a package-level name or a field somewhere
}

func newGoIndex() *goIndex {
	return &goIndex{names: map[string]map[string]bool{}, funcs: map[string]map[string]bool{}, types: map[string]map[string]*goType{},
		byName: map[string][]*goType{}, all: map[string]bool{}, other: map[string]bool{}}
}

// methodOnly reports whether n is a method and nothing else: no type,
// function, constant, variable or field has that name.
func (x *goIndex) methodOnly(n string) bool {
	return x.all[n] && !x.other[n]
}

// isMethod and isField report whether t has m as a method or a field,
// promoted members included.
func (x *goIndex) isMethod(t *goType, m string) bool {
	f, ok := x.member(t, m, map[*goType]bool{})
	return ok && f == ""
}

func (x *goIndex) isField(t *goType, m string) bool {
	f, ok := x.member(t, m, map[*goType]bool{})
	return ok && f != ""
}

var (
	goLineRe   = regexp.MustCompile(`^pkg ([^,]+), (.*)$`)
	memberRe   = regexp.MustCompile(`^type (\w+)(?:\[[^\]]*\])? (?:struct|interface), (.*)$`)
	typeRe     = regexp.MustCompile(`^type (\w+)`)
	aliasRe    = regexp.MustCompile(`^type \w+ = (\S+)`)
	methodRe   = regexp.MustCompile(`^method \(\*?(\w+)(?:\[[^\]]*\])?\) (\w+)`)
	declRe     = regexp.MustCompile(`^(const|var|func) (\w+)`)
	memberName = regexp.MustCompile(`^(\w+)`)
	tagSuffix  = regexp.MustCompile("\\s+`[^`]*`$")
)

func parseGoTxt(text string) *goIndex {
	x := newGoIndex()
	typ := func(pkg, name string) *goType {
		if x.types[pkg] == nil {
			x.types[pkg] = map[string]*goType{}
		}
		t := x.types[pkg][name]
		if t == nil {
			t = &goType{pkg: pkg, name: name, members: map[string]string{}}
			x.types[pkg][name] = t
			x.byName[name] = append(x.byName[name], t)
		}
		return t
	}
	name := func(pkg, n string) {
		if x.names[pkg] == nil {
			x.names[pkg] = map[string]bool{}
		}
		x.names[pkg][n] = true
		x.all[n] = true
		x.other[n] = true
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := goLineRe.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		pkg := m[1][strings.LastIndexByte(m[1], '/')+1:]
		rest := m[2]
		switch {
		case memberRe.MatchString(rest):
			mm := memberRe.FindStringSubmatch(rest)
			t := typ(pkg, mm[1])
			name(pkg, mm[1])
			if e, ok := strings.CutPrefix(mm[2], "embedded "); ok {
				t.embedded = append(t.embedded, e)
				continue
			}
			if n := memberName.FindString(mm[2]); n != "" {
				field := strings.TrimSpace(tagSuffix.ReplaceAllString(strings.TrimPrefix(mm[2], n), ""))
				if strings.HasPrefix(field, "(") {
					field = "" // an interface method
				}
				t.members[n] = field
				x.all[n] = true
				x.other[n] = x.other[n] || field != ""
			}
		case methodRe.MatchString(rest):
			mm := methodRe.FindStringSubmatch(rest)
			typ(pkg, mm[1]).members[mm[2]] = ""
			x.all[mm[2]] = true
		case typeRe.MatchString(rest):
			n := typeRe.FindStringSubmatch(rest)[1]
			t := typ(pkg, n)
			name(pkg, n)
			if a := aliasRe.FindStringSubmatch(rest); a != nil {
				t.alias = a[1]
			}
		case declRe.MatchString(rest):
			d := declRe.FindStringSubmatch(rest)
			name(pkg, d[2])
			if d[1] == "func" {
				x.addFunc(pkg, d[2])
			}
		}
	}
	return x
}

func (x *goIndex) addFunc(pkg, n string) {
	if x.funcs[pkg] == nil {
		x.funcs[pkg] = map[string]bool{}
	}
	x.funcs[pkg][n] = true
}

// resolve finds the type a field or embedded type string names, from pkg.
func (x *goIndex) resolve(pkg, ref string) *goType {
	ref = strings.TrimLeft(ref, "*[]")
	if i := strings.LastIndexByte(ref, ']'); strings.HasPrefix(ref, "map[") && i > 0 {
		ref = strings.TrimLeft(ref[i+1:], "*[]")
	}
	if i := strings.IndexAny(ref, "[ "); i >= 0 {
		ref = ref[:i]
	}
	q, n, qualified := strings.Cut(ref, ".")
	if !qualified {
		return x.types[pkg][q]
	}
	if t := x.types[q][n]; t != nil {
		return t
	}
	if t := x.types["openrails"][n]; t != nil { // an internal type the root aliases
		return t
	}
	if c := x.byName[n]; len(c) == 1 {
		return c[0]
	}
	return nil
}

// member reports whether t has m, promoted members included.
func (x *goIndex) member(t *goType, m string, seen map[*goType]bool) (string, bool) {
	if seen[t] {
		return "", false
	}
	seen[t] = true
	if f, ok := t.members[m]; ok {
		return f, true
	}
	for _, e := range t.embedded {
		if et := x.resolve(t.pkg, e); et != nil {
			if f, ok := x.member(et, m, seen); ok {
				return f, true
			}
		}
	}
	return "", false
}

// chain checks members after type t; it stops (accepting) where a field's
// type cannot be resolved, or at an alias to a type it does not list.
func (x *goIndex) chain(t *goType, rest []string) (string, bool) {
	path := t.pkg + "." + t.name
	for _, m := range rest {
		f, ok := x.member(t, m, map[*goType]bool{})
		if !ok && t.alias != "" && len(t.members) == 0 && len(t.embedded) == 0 {
			q, n, _ := strings.Cut(t.alias, ".")
			if c := x.types[q][n]; c != nil && c != t && len(c.members) > 0 {
				return x.chain(c, rest)
			}
			for _, c := range x.byName[n] {
				if c != t && len(c.members) > 0 {
					return x.chain(c, rest)
				}
			}
			return path, true
		}
		if !ok {
			return path + "." + m, false
		}
		path += "." + m
		if t = x.resolve(t.pkg, f); t == nil {
			return path, true
		}
	}
	return path, true
}

// addGoSource indexes the exported names of the Go packages in dirs (an
// external module's API, such as AuthKit's), under each package's name.
func (x *goIndex) addGoSource(dirs ...string) error {
	fset := token.NewFileSet()
	for _, dir := range dirs {
		pkgs, err := parseGoDir(fset, dir)
		if err != nil {
			return err
		}
		for pkg, files := range pkgs {
			add := func(n string) {
				if x.names[pkg] == nil {
					x.names[pkg] = map[string]bool{}
				}
				x.names[pkg][n] = true
				x.all[n] = true
				x.other[n] = true
			}
			typ := func(n string) *goType {
				if x.types[pkg] == nil {
					x.types[pkg] = map[string]*goType{}
				}
				t := x.types[pkg][n]
				if t == nil {
					t = &goType{pkg: pkg, name: n, members: map[string]string{}}
					x.types[pkg][n] = t
					x.byName[n] = append(x.byName[n], t)
				}
				return t
			}
			for _, f := range files {
				for _, decl := range f.Decls {
					switch d := decl.(type) {
					case *ast.FuncDecl:
						if !d.Name.IsExported() {
							continue
						}
						if d.Recv == nil {
							add(d.Name.Name)
							x.addFunc(pkg, d.Name.Name)
							continue
						}
						recv := d.Recv.List[0].Type
						if s, ok := recv.(*ast.StarExpr); ok {
							recv = s.X
						}
						if id, ok := recv.(*ast.Ident); ok {
							typ(id.Name).members[d.Name.Name] = ""
							x.all[d.Name.Name] = true
						}
					case *ast.GenDecl:
						for _, spec := range d.Specs {
							switch sp := spec.(type) {
							case *ast.ValueSpec:
								for _, n := range sp.Names {
									if n.IsExported() {
										add(n.Name)
									}
								}
							case *ast.TypeSpec:
								if !sp.Name.IsExported() {
									continue
								}
								add(sp.Name.Name)
								t := typ(sp.Name.Name)
								var fields *ast.FieldList
								iface := false
								switch tt := sp.Type.(type) {
								case *ast.StructType:
									fields = tt.Fields
								case *ast.InterfaceType:
									fields, iface = tt.Methods, true
								}
								if fields == nil {
									continue
								}
								for _, fl := range fields.List {
									for _, n := range fl.Names {
										if !n.IsExported() {
											continue
										}
										t.members[n.Name] = ""
										if !iface {
											t.members[n.Name] = types.ExprString(fl.Type)
											x.other[n.Name] = true
										}
										x.all[n.Name] = true
									}
									if len(fl.Names) == 0 {
										t.embedded = append(t.embedded, types.ExprString(fl.Type))
									}
								}
							}
						}
					}
				}
			}
		}
	}
	return nil
}

var tsExport = regexp.MustCompile(`export\s+(?:default\s+)?(?:async\s+)?(?:function|const|class|type|interface|enum)\s+([A-Z]\w*)|export\s+(?:type\s+)?\{([^}]*)\}`)

// addTSExports indexes the names TypeScript entry points export (React
// components of billing-ui and auth-ui).
func (x *goIndex) addTSExports(texts ...string) {
	for _, text := range texts {
		for _, m := range tsExport.FindAllStringSubmatch(text, -1) {
			if m[1] != "" {
				x.all[m[1]], x.other[m[1]] = true, true
			}
			for _, part := range strings.Split(m[2], ",") {
				f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(part), "type "))
				if len(f) > 0 {
					x.all[f[len(f)-1]], x.other[f[len(f)-1]] = true, true
				}
			}
		}
	}
}
