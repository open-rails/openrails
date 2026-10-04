// Package contract renders the HTTP contract from the route catalog
// (internal/http/routes) and the error-code registry (billing.ErrorCodes):
// api/openapi.json, the TypeScript wire types of billing-ui and the admin
// console, and the route and error tables under docs/api. Nothing here is
// written by hand twice: `go run ./scripts/contracts -write` regenerates the
// files and TestGeneratedContractIsFresh fails when one is stale.
package contract

import (
	"encoding"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
)

const module = "github.com/open-rails/openrails"

// kind is how a Go type appears on the wire.
type kind int

const (
	kindString kind = iota
	kindInteger
	kindNumber
	kindBoolean
	kindTime
	kindAny
	kindArray
	kindMap
	kindObject
	kindPage
	kindStream
)

var (
	timeType          = reflect.TypeFor[time.Time]()
	rawMessageType    = reflect.TypeFor[json.RawMessage]()
	untypedType       = reflect.TypeFor[routes.Untyped]()
	streamType        = reflect.TypeFor[routes.Stream]()
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// CardEntry is the request form of a card (billing.Card): it decodes from
// this object and never encodes.
type CardEntry struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	CVC      string `json:"cvc"`
}

// wireForm is the type a self-marshaling type is on the wire. A type with a
// MarshalJSON that has no entry fails generation: say what it writes.
func wireForm(t reflect.Type) (reflect.Type, bool) {
	name := t.String()
	switch {
	case name == "cardguard.Card":
		return reflect.TypeFor[CardEntry](), true
	case name == "billing.AmountMap":
		return reflect.TypeFor[map[string]string](), true
	case name == "billing.MerchantConfigurationApplyParams":
		// Its codec only enforces strictness; the members are the wire.
		return t, true
	case strings.HasPrefix(name, "catalog.Field["):
		// A declared, omitted or null value: T on the wire, an int64 (money)
		// as a decimal string.
		value, _ := t.FieldByName("Value")
		if value.Type.Kind() == reflect.Int64 {
			return reflect.TypeFor[string](), true
		}
		return value.Type, true
	}
	return nil, false
}

func isListPage(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t.PkgPath() == module+"/billing" && strings.HasPrefix(t.Name(), "ListPage[")
}

// classify is t's wire kind, pointers looked through.
func classify(t reflect.Type) (kind, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return kindTime, nil
	case t == rawMessageType, t == untypedType:
		return kindAny, nil
	case t == streamType:
		return kindStream, nil
	case isListPage(t):
		return kindPage, nil
	}
	if t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) {
		form, ok := wireForm(t)
		if !ok {
			return 0, fmt.Errorf("contract: %s writes its own JSON; declare what in wireForm", t)
		}
		if form != t {
			return classify(form)
		}
		return kindObject, nil
	}
	if t.Implements(textMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType) {
		return kindString, nil
	}
	switch t.Kind() {
	case reflect.String:
		return kindString, nil
	case reflect.Bool:
		return kindBoolean, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return kindInteger, nil
	case reflect.Float32, reflect.Float64:
		return kindNumber, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return kindString, nil // base64
		}
		return kindArray, nil
	case reflect.Map:
		return kindMap, nil
	case reflect.Interface:
		return kindAny, nil
	case reflect.Struct:
		return kindObject, nil
	}
	return 0, fmt.Errorf("contract: %s has no wire form", t)
}

// field is one JSON member of an object.
type field struct {
	name     string
	t        reflect.Type
	optional bool // omitempty or omitzero: absent when empty
	nullable bool // null when unset: a pointer, slice or map written without omitempty
	quoted   bool // `,string`: a number written as a string
}

// fieldsOf lists t's JSON members the way encoding/json writes them:
// embedded structs flattened, unexported and "-" members skipped.
func fieldsOf(t reflect.Type) []field {
	var out []field
	seen := map[string]int{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		for i := range t.NumField() {
			f := t.Field(i)
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if f.Anonymous && name == "" && ft.Kind() == reflect.Struct && ft != timeType {
				if k, err := classify(ft); err == nil && k == kindObject {
					walk(ft)
					continue
				}
			}
			if !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			member := field{name: name, t: f.Type}
			for _, opt := range strings.Split(opts, ",") {
				switch opt {
				case "omitempty", "omitzero":
					member.optional = true
				case "string":
					member.quoted = true
				}
			}
			switch f.Type.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
				member.nullable = !member.optional
			}
			if strings.HasPrefix(f.Type.String(), "catalog.Field[") {
				member.nullable = true
			}
			if at, dup := seen[name]; dup {
				out[at] = member // the outer member shadows the embedded one
				continue
			}
			seen[name] = len(out)
			out = append(out, member)
		}
	}
	walk(t)
	return out
}

// object is a named struct on the wire.
type object struct {
	name   string
	t      reflect.Type
	fields []field
	input  bool // reached from a request body
	output bool // reached from a response body
}

// model is every wire type a set of routes reaches.
type model struct {
	routes  []routes.Route
	objects map[string]*object
	names   map[reflect.Type]string
	enums   *enumIndex
}

// elem is t with pointers removed, in its wire form.
func elem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(jsonMarshalerType) {
		if form, ok := wireForm(t); ok && form != t {
			return elem(form)
		}
	}
	return t
}

// pageItem is the item type of a billing.ListPage.
func pageItem(t reflect.Type) reflect.Type {
	items, _ := elem(t).FieldByName("Items")
	return items.Type.Elem()
}

// baseName is a struct's wire name: its Go name without its package,
// capitalized; generic arguments become a suffix (Page[billing.Subscription]
// is PageOfSubscription).
func baseName(t reflect.Type) string {
	name := strings.ToUpper(t.Name()[:1]) + t.Name()[1:]
	open := strings.IndexByte(name, '[')
	if open < 0 {
		return name
	}
	var args []string
	for _, arg := range strings.Split(name[open+1:len(name)-1], ",") {
		arg = strings.TrimLeft(arg, "*[]")
		if dot := strings.LastIndexByte(arg, '.'); dot >= 0 {
			arg = arg[dot+1:]
		}
		args = append(args, strings.ToUpper(arg[:1])+arg[1:])
	}
	return name[:open] + "Of" + strings.Join(args, "And")
}

// camel is a JSON member name as a type-name part: line_items is LineItems.
func camel(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		switch {
		case r == '_' || r == '-' || r == '.':
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func packageName(t reflect.Type) string {
	name := path.Base(t.PkgPath())
	return strings.ToUpper(name[:1]) + name[1:]
}

func newModel(fsys fs.FS, list []routes.Route) (*model, error) {
	m := &model{routes: list, objects: map[string]*object{}, names: map[reflect.Type]string{}, enums: &enumIndex{fsys: fsys, values: map[string][]string{}}}
	// Name every reached struct first: a bare name when one type holds it,
	// its package's name in front when two do.
	// reached maps each struct to its base name. A nested anonymous struct is
	// named after the member that holds it.
	reached := map[reflect.Type]string{}
	var find func(t reflect.Type, hint string) error
	find = func(t reflect.Type, hint string) error {
		t = elem(t)
		k, err := classify(t)
		if err != nil {
			return err
		}
		switch k {
		case kindArray, kindMap:
			return find(t.Elem(), hint)
		case kindPage:
			return find(pageItem(t), hint)
		case kindObject:
			if _, ok := reached[t]; ok {
				return nil
			}
			name := hint
			if t.Name() != "" {
				name = baseName(t)
			} else if name == "" {
				return fmt.Errorf("contract: anonymous struct %s as a body; name it or declare the route Untyped", t)
			}
			reached[t] = name
			for _, f := range fieldsOf(t) {
				if err := find(f.t, name+camel(f.name)); err != nil {
					return fmt.Errorf("%s.%s: %w", t, f.name, err)
				}
			}
		}
		return nil
	}
	bodies := func(visit func(t reflect.Type, input bool) error) error {
		for _, r := range list {
			if r.Request != nil {
				if err := visit(reflect.TypeOf(r.Request), true); err != nil {
					return fmt.Errorf("%s request: %w", r.Key(), err)
				}
			}
			for _, reply := range r.Responses {
				if reply.Body != nil {
					if err := visit(reflect.TypeOf(reply.Body), false); err != nil {
						return fmt.Errorf("%s response: %w", r.Key(), err)
					}
				}
			}
		}
		return nil
	}
	if err := bodies(func(t reflect.Type, _ bool) error { return find(t, "") }); err != nil {
		return nil, err
	}
	byBase := map[string][]reflect.Type{}
	for t, base := range reached {
		byBase[base] = append(byBase[base], t)
	}
	for base, types := range byBase {
		for _, t := range types {
			name := base
			if len(types) > 1 && t.PkgPath() != module+"/billing" {
				if t.PkgPath() == "" {
					return nil, fmt.Errorf("contract: anonymous struct %s shares the name %s; name it", t, base)
				}
				name = packageName(t) + base
			}
			if other, dup := m.objects[name]; dup {
				return nil, fmt.Errorf("contract: %s and %s are both named %s on the wire", other.t, t, name)
			}
			m.names[t] = name
			m.objects[name] = &object{name: name, t: t, fields: fieldsOf(t)}
		}
	}
	// Then mark which side of the wire each is on.
	var mark func(t reflect.Type, input bool)
	mark = func(t reflect.Type, input bool) {
		t = elem(t)
		switch k, _ := classify(t); k {
		case kindArray, kindMap:
			mark(t.Elem(), input)
		case kindPage:
			mark(pageItem(t), input)
		case kindObject:
			o := m.objects[m.names[t]]
			if input && o.input || !input && o.output {
				return
			}
			if input {
				o.input = true
			} else {
				o.output = true
			}
			for _, f := range o.fields {
				mark(f.t, input)
			}
		}
	}
	_ = bodies(func(t reflect.Type, input bool) error { mark(t, input); return nil })
	return m, nil
}

func (m *model) sortedObjects() []*object {
	out := make([]*object, 0, len(m.objects))
	for _, o := range m.objects {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// enumIndex finds the string constants declared with a named string type:
// the values its wire field takes.
type enumIndex struct {
	fsys   fs.FS
	values map[string][]string
}

func (e *enumIndex) of(t reflect.Type) []string {
	t = elem(t)
	if t.Kind() != reflect.String || t.Name() == "" || !strings.HasPrefix(t.PkgPath(), module) {
		return nil
	}
	key := t.PkgPath() + "." + t.Name()
	if values, ok := e.values[key]; ok {
		return values
	}
	dir := strings.TrimPrefix(strings.TrimPrefix(t.PkgPath(), module), "/")
	if dir == "" {
		dir = "."
	}
	var out []string
	entries, err := fs.ReadDir(e.fsys, dir)
	if err != nil {
		panic(fmt.Sprintf("contract: read %s: %v", dir, err))
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := fs.ReadFile(e.fsys, path.Join(dir, name))
		if err != nil {
			panic(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, body, parser.SkipObjectResolution)
		if err != nil {
			panic(fmt.Sprintf("contract: parse %s: %v", name, err))
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != t.Name() {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						value, _ := strconv.Unquote(lit.Value)
						out = append(out, value)
					}
				}
			}
		}
	}
	sort.Strings(out)
	e.values[key] = out
	return out
}

// pathParams lists a route path's {wildcards}.
func pathParams(p string) []string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, seg[1:len(seg)-1])
		}
	}
	return out
}

// errorCodes is the registry, sorted by code.
func errorCodes() []billing.ErrorCode { return billing.ErrorCodes() }
