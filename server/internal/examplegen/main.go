// examplegen writes the repository's two configuration references from the
// configuration types: .env.example, every environment variable the openrails
// binary reads (hostconfig.Settings), and merchant.example.yaml, every key of
// a merchant's document (billing.MerchantDocument, with a PSP of each rail and
// a custodian of each kind). Descriptions are the fields' doc comments.
// Without -write it fails when either file differs. Run it from server/.
package main

import (
	"bytes"
	"context"
	"encoding"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

func main() {
	write := flag.Bool("write", false, "rewrite the files")
	root := flag.String("root", "..", "the repository root")
	flag.Parse()
	if err := run(*root, *write); err != nil {
		fmt.Fprintln(os.Stderr, "examplegen:", err)
		os.Exit(1)
	}
}

func run(root string, write bool) error {
	settings, err := hostconfig.Settings()
	if err != nil {
		return err
	}
	d, err := loadDocs(settings)
	if err != nil {
		return err
	}
	env, err := envExample(settings, d)
	if err != nil {
		return err
	}
	merchant, err := merchantExample(d)
	if err != nil {
		return err
	}
	var stale []string
	for name, body := range map[string][]byte{".env.example": env, "merchant.example.yaml": merchant} {
		path := filepath.Join(root, name)
		if write {
			if err := os.WriteFile(path, body, 0o644); err != nil {
				return err
			}
			continue
		}
		if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, body) {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		slices.Sort(stale)
		return fmt.Errorf("%s differ from the configuration types: run `go run ./internal/examplegen -write` in server/", strings.Join(stale, " and "))
	}
	return nil
}

// docs are struct fields' doc comments by "pkgpath.Type.Field", the first
// sentence of each.
type docs map[string]string

// authkitDefaults are defaults AuthKit applies itself and exports none of.
var authkitDefaults = map[string]string{"auth.keys.path": "/vault/auth"}

// overrides replace a field's comment where the reference says more.
var overrides = map[string]string{}

func (d docs) of(owner reflect.Type, field string) string {
	if owner == nil {
		return ""
	}
	return d[owner.PkgPath()+"."+owner.Name()+"."+field]
}

// loadDocs reads the comments of every package a setting or a merchant
// document field is declared in.
func loadDocs(settings []hostconfig.Setting) (docs, error) {
	pkgs := map[string]bool{}
	for _, s := range settings {
		if s.Owner != nil {
			pkgs[s.Owner.PkgPath()] = true
		}
	}
	collect(reflect.TypeFor[billing.MerchantDocument](), pkgs, map[reflect.Type]bool{})
	for _, t := range pspTypes {
		pkgs[t.PkgPath()] = true
	}
	list := slices.Sorted(func(yield func(string) bool) {
		for p := range pkgs {
			if !yield(p) {
				return
			}
		}
	})
	out, err := exec.CommandContext(context.Background(), "go", append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \" \"}}"}, list...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	d := docs{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pkg, rest, _ := strings.Cut(line, "\t")
		dir, names, _ := strings.Cut(rest, "\t")
		fset := token.NewFileSet()
		for _, name := range strings.Fields(names) {
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
			if err != nil {
				return nil, err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					return true
				}
				previous := ""
				for _, field := range st.Fields.List {
					doc := field.Doc.Text()
					if doc == "" {
						doc = field.Comment.Text()
					}
					for _, name := range field.Names {
						text := doc
						if text == "" && strings.Contains(previous, name.Name) {
							text = previous // "Host and Port are …"
						}
						if text != "" {
							d[pkg+"."+ts.Name.Name+"."+name.Name] = sentence(text)
						}
					}
					if doc != "" {
						previous = doc
					}
				}
				return false
			})
		}
	}
	return d, nil
}

// collect adds the packages of t's struct types, through fields, elements and
// map values.
func collect(t reflect.Type, pkgs map[string]bool, seen map[reflect.Type]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	pkgs[t.PkgPath()] = true
	for i := 0; i < t.NumField(); i++ {
		collect(t.Field(i).Type, pkgs, seen)
	}
}

var spaces = regexp.MustCompile(`\s+`)

// sentence is the first sentence of a comment, on one line.
func sentence(doc string) string {
	doc = strings.TrimSpace(spaces.ReplaceAllString(doc, " "))
	for i := 0; i < len(doc); i++ {
		if doc[i] == '.' && (i+1 == len(doc) || doc[i+1] == ' ') && !strings.HasSuffix(doc[:i], "e.g") && !strings.HasSuffix(doc[:i], "i.e") {
			return doc[:i+1]
		}
	}
	return doc + "."
}

// envExample renders .env.example.
func envExample(settings []hostconfig.Setting, d docs) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`# Every environment variable the openrails binary reads: a reference, read by
# nothing. Generated from the configuration types; regenerate with
# ` + "`go run ./internal/examplegen -write`" + ` in server/. Each value is the default
# (empty: none) and each secret is empty. A variable is its config.yaml key,
# upper-cased and joined by "_"; a list is comma-separated, a section or a
# list of sections JSON. A variable inside a known section that names no key
# refuses boot. A merchant's settings are no variables: its document is
# merchant.example.yaml.
`)
	var order []string
	sections := map[string][]string{}
	var missing []string
	for _, s := range settings {
		name := "process"
		if s.Path != "" {
			name = "top level"
			if head, _, nested := strings.Cut(s.Path, "."); nested {
				name = head
			}
		}
		if _, ok := sections[name]; !ok {
			order = append(order, name)
		}
		doc := s.Doc
		if doc == "" {
			doc = overrides[s.Path]
		}
		if doc == "" {
			doc = d.of(s.Owner, s.Field)
		}
		if doc == "" && strings.HasPrefix(s.Path, "auth.") {
			doc = "AuthKit's " + s.Path + "."
		}
		if doc == "" {
			missing = append(missing, s.Path)
		}
		value := s.Default
		if value == "" {
			value = authkitDefaults[s.Path]
		}
		sections[name] = append(sections[name], fmt.Sprintf("# %s\n%s=%s\n", doc, s.Env, value))
	}
	for _, name := range order {
		fmt.Fprintf(&b, "\n# --- %s ---\n%s", name, strings.Join(sections[name], ""))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("settings without a doc comment: %s", strings.Join(missing, ", "))
	}
	return b.Bytes(), nil
}

// pspTypes are the typed PSP declarations, one per rail, whose psp tags are
// exactly a PSP's fields.
var pspTypes = []reflect.Type{reflect.TypeFor[openrails.NMIPSP](), reflect.TypeFor[openrails.StripePSP](), reflect.TypeFor[openrails.CCBillPSP](), reflect.TypeFor[openrails.SolanaPSP]()}

// merchantExample renders merchant.example.yaml.
func merchantExample(d docs) ([]byte, error) {
	w := &yamlWriter{d: d}
	w.b.WriteString(`# One merchant's document, with every key: a reference, read by nothing.
# Generated from billing.MerchantDocument; regenerate with
# ` + "`go run ./internal/examplegen -write`" + ` in server/. A key left out keeps
# its default, which its description names; values here are empty. A file
# document names its slug; a merchant manifest keys each document by it under
# merchants.<slug>, and with vault.kv_mount Vault holds them. Secrets belong
# in a manifest overlay or Vault. A list shows one element, a map one <key>.
`)
	w.fields(reflect.TypeFor[billing.MerchantDocument](), 0, map[reflect.Type]bool{})
	if len(w.missing) > 0 {
		slices.Sort(w.missing)
		return nil, fmt.Errorf("merchant document fields without a doc comment: %s", strings.Join(slices.Compact(w.missing), ", "))
	}
	return w.b.Bytes(), nil
}

type yamlWriter struct {
	b       bytes.Buffer
	d       docs
	missing []string
}

var (
	pspsType       = reflect.TypeFor[billing.PSPs]()
	custodiansType = reflect.TypeFor[billing.Custodians]()
	textUnmarshal  = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// key is a field's document key: its yaml tag, else its json tag; quoted
// reports a number written as a string (json ",string").
func key(f reflect.StructField) (name string, inline, quoted bool) {
	tag, ok := f.Tag.Lookup("yaml")
	if !ok {
		tag = f.Tag.Get("json")
	}
	name, opts, _ := strings.Cut(tag, ",")
	options := strings.Split(opts, ",")
	return name, slices.Contains(options, "inline") || f.Anonymous && name == "", slices.Contains(options, "string")
}

func (w *yamlWriter) line(depth int, text, doc string) {
	if doc != "" {
		fmt.Fprintf(&w.b, "%s# %s\n", strings.Repeat("  ", depth), doc)
	}
	fmt.Fprintf(&w.b, "%s%s\n", strings.Repeat("  ", depth), text)
}

func (w *yamlWriter) fields(t reflect.Type, depth int, seen map[reflect.Type]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, inline, quoted := key(f)
		if inline {
			w.fields(deref(f.Type), depth, seen)
			continue
		}
		if name == "" || name == "-" {
			continue
		}
		doc := w.d.of(t, f.Name)
		if doc == "" {
			w.missing = append(w.missing, t.Name()+"."+f.Name)
		}
		if quoted {
			w.line(depth, name+`: "0"`, doc)
			continue
		}
		w.value(name, doc, f.Type, depth, seen)
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// scalar reports a type a document writes as one value.
func scalar(t reflect.Type) bool {
	if t.Implements(textUnmarshal) || reflect.PointerTo(t).Implements(textUnmarshal) {
		return true
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Interface:
		return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
	}
	return true
}

func zero(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return "0"
	}
	return `""`
}

func (w *yamlWriter) value(name, doc string, t reflect.Type, depth int, seen map[reflect.Type]bool) {
	t = deref(t)
	switch {
	case t == pspsType:
		w.line(depth, name+":", doc)
		w.psps(depth + 1)
	case t == custodiansType:
		w.line(depth, name+":", doc)
		w.custodians(depth + 1)
	case scalar(t):
		w.line(depth, name+": "+zero(t), doc)
	case t.Kind() == reflect.Struct:
		if seen[t] {
			w.line(depth, name+": {}", doc)
			return
		}
		w.line(depth, name+":", doc)
		seen[t] = true
		w.fields(t, depth+1, seen)
		delete(seen, t)
	case t.Kind() == reflect.Slice:
		elem := deref(t.Elem())
		if scalar(elem) {
			w.line(depth, name+": []", doc)
			return
		}
		w.line(depth, name+":", doc)
		if elem.Kind() == reflect.Struct && !seen[elem] {
			seen[elem] = true
			w.listItem(elem, depth+1, seen)
			delete(seen, elem)
		}
	case t.Kind() == reflect.Map:
		elem := deref(t.Elem())
		if scalar(elem) || elem.Kind() == reflect.Interface {
			w.line(depth, name+": {}", doc)
			return
		}
		w.line(depth, name+":", doc)
		if elem.Kind() == reflect.Struct && !seen[elem] {
			seen[elem] = true
			w.line(depth+1, "<key>:", "")
			w.fields(elem, depth+2, seen)
			delete(seen, elem)
		}
	default:
		w.line(depth, name+": {}", doc)
	}
}

// listItem writes one element of a list of t, its first key on the dash.
func (w *yamlWriter) listItem(t reflect.Type, depth int, seen map[reflect.Type]bool) {
	item := &yamlWriter{d: w.d}
	item.fields(t, depth+1, seen)
	w.missing = append(w.missing, item.missing...)
	indent := strings.Repeat("  ", depth+1)
	dashed := false
	for _, l := range strings.Split(strings.TrimSuffix(item.b.String(), "\n"), "\n") {
		if !dashed && !strings.HasPrefix(strings.TrimSpace(l), "#") {
			fmt.Fprintf(&w.b, "%s- %s\n", strings.Repeat("  ", depth), strings.TrimPrefix(l, indent))
			dashed = true
			continue
		}
		fmt.Fprintln(&w.b, l)
	}
}

// psps writes one PSP of each rail, keyed by the rail, from its typed
// declaration: rail, then its fields, then its secrets.
func (w *yamlWriter) psps(depth int) {
	for _, t := range pspTypes {
		rail := strings.ToLower(strings.TrimSuffix(t.Name(), "PSP"))
		w.line(depth, rail+":", "")
		w.line(depth+1, "rail: "+rail, "The rail this PSP is on.")
		var secrets []reflect.StructField
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("psp"), ",")
			if tag == "" {
				continue
			}
			if strings.HasPrefix(tag, "secrets.") {
				secrets = append(secrets, f)
				continue
			}
			doc := w.d.of(t, f.Name)
			if doc == "" {
				w.missing = append(w.missing, t.Name()+"."+f.Name)
			}
			w.line(depth+1, tag+": "+zero(f.Type), doc)
		}
		if len(secrets) > 0 {
			w.line(depth+1, "secrets:", "Its credentials.")
			for _, f := range secrets {
				tag, opts, _ := strings.Cut(f.Tag.Get("psp"), ",")
				doc := w.d.of(t, f.Name)
				if doc == "" {
					w.missing = append(w.missing, t.Name()+"."+f.Name)
				}
				if opts == "required" {
					doc = strings.TrimSuffix(doc, ".") + ". Required."
				}
				w.line(depth+2, strings.TrimPrefix(tag, "secrets.")+": \"\"", doc)
			}
		}
	}
}

// custodians writes one custodian of each kind, keyed by the kind.
func (w *yamlWriter) custodians(depth int) {
	t := reflect.TypeFor[billing.CustodianConfig]()
	for _, kind := range custodians.Kinds() {
		desc, _ := custodians.Require(kind)
		w.line(depth, kind+":", "")
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := key(f)
			doc := w.d.of(t, f.Name)
			switch name {
			case "kind":
				w.line(depth+1, "kind: "+kind, doc)
			case "settings":
				w.line(depth+1, "settings:", doc)
				for _, s := range desc.Settings {
					value := `""`
					switch s.Kind {
					case custodians.ValueBool:
						value = "false"
					case custodians.ValueInt:
						value = "0"
					}
					w.line(depth+2, s.Name+": "+value, "")
				}
			case "secrets":
				w.line(depth+1, "secrets:", doc)
				for _, s := range desc.Secrets {
					note := ""
					if s.Required {
						note = "Required."
					}
					w.line(depth+2, s.Name+": \"\"", note)
				}
			default:
				w.line(depth+1, name+": "+zero(f.Type), doc)
			}
		}
	}
}
