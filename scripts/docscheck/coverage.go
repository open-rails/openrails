package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
)

// Coverage is what the reference pages must each name.
type Coverage struct {
	Methods    map[string][]string // method or func name to its owners
	ConfigKeys []string            // server configuration key paths
	HelmKeys   []string            // the chart's top-level values
	Commands   []string            // operator CLI command paths

	// What exists, for the reverse rule: every command path (hidden ones
	// included), and every configuration and chart key path with its
	// prefixes, commented-out examples included.
	AllCommands map[string]bool
	ConfigPaths map[string]bool // normalized (normKey) per segment
	ConfigMaps  map[string]bool // normalized paths whose keys are free
	HelmPaths   map[string]bool
}

const cCoverage = "coverage"

const (
	clientPage, embeddedPage, standalonePage = "docs/client.md", "docs/embedded.md", "docs/standalone.md"
)

var (
	coveredMethod = regexp.MustCompile(`^pkg openrails, method \(\*?(Client|LocalClient|Engine|Remote|Gate|Ticket)\) (\w+)\(`)
	coveredFunc   = regexp.MustCompile(`^pkg openrails, func (\w+)\(`)
	upperToken    = regexp.MustCompile(`[A-Z]\w*`)
)

// collapsed are configuration maps keyed by the operator's own names,
// covered at their parent: rate_limits buckets are named by limiter.
var collapsed = []string{"rate_limits"}

// hiddenCommands are CLI commands that are no operator's: loopback provider
// fakes for disposable test stacks.
var hiddenCommands = map[string]bool{"sandbox": true, "sandbox nmi-gateway": true}

// freeForm are sections whose keys are someone else's or the operator's own
// names, so a documented key beneath them is not checked: AuthKit's auth
// section, rate-limit buckets, console issuers by merchant, and the chart's
// config (config.yaml itself, checked as configuration keys) and files.
var freeForm = []string{"auth", "rate_limits", "admin_console.issuers", "config", "files"}

func underFreeForm(k string) bool {
	for _, f := range freeForm {
		if strings.HasPrefix(k, f+".") {
			return true
		}
	}
	return false
}

// checkCoverage reports what the reference pages leave out.
func checkCoverage(cov *Coverage, load func(string) *doc, add func(pos, class, format string, args ...any)) {
	tokens := map[string]bool{}
	for _, p := range []string{clientPage, embeddedPage} {
		if d := load(p); d != nil {
			for _, s := range d.Spans {
				for _, t := range upperToken.FindAllString(s.Text, -1) {
					tokens[t] = true
				}
			}
		}
	}
	names := make([]string, 0, len(cov.Methods))
	for n := range cov.Methods {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !tokens[n] {
			add(clientPage, cCoverage, "%s (%s) is named in neither %s nor %s", n, strings.Join(cov.Methods[n], ", "), clientPage, embeddedPage)
		}
	}

	d := load(standalonePage)
	var spans, fences, paths []string
	if d != nil {
		for _, s := range d.Spans {
			spans = append(spans, strings.TrimSpace(s.Text))
		}
		for _, fe := range d.Fences {
			text := fenceText(d, fe)
			fences = append(fences, text)
			if fe.Lang == "yaml" || fe.Lang == "yml" {
				paths = append(paths, yamlPaths([]byte(text))...)
			}
		}
	}
	leaves := map[string]int{}
	for _, k := range cov.ConfigKeys {
		leaves[k[strings.LastIndexByte(k, '.')+1:]]++
	}
	keyPresent := func(k string, bare bool) bool {
		for _, s := range spans {
			if s == k || strings.HasPrefix(s, k) && strings.ContainsRune(".:= [", rune(s[len(k)])) {
				return true
			}
			if bare && s == k[strings.LastIndexByte(k, '.')+1:] {
				return true
			}
		}
		for _, p := range paths {
			if p == k || strings.HasPrefix(p, k+".") {
				return true
			}
		}
		return false
	}
	for _, k := range cov.ConfigKeys {
		if !keyPresent(k, leaves[k[strings.LastIndexByte(k, '.')+1:]] == 1) {
			add(standalonePage, cCoverage, "configuration key %s is not in %s", k, standalonePage)
		}
	}
	for _, k := range cov.HelmKeys {
		if !keyPresent(k, false) {
			add(standalonePage, cCoverage, "chart value %s is not in %s", k, standalonePage)
		}
	}
	text := append(spans, fences...)
	for _, c := range cov.Commands {
		re := regexp.MustCompile(`(^|[^\w-])` + regexp.QuoteMeta(c) + `($|[^\w-])`)
		found := false
		for _, t := range text {
			if re.MatchString(t) {
				found = true
				break
			}
		}
		if !found {
			add(standalonePage, cCoverage, "command `openrails %s` is not in %s", c, standalonePage)
		}
	}
	if d != nil {
		checkStale(cov, d, add)
	}
}

const cAbsent = "absent"

var (
	// cliUse finds openrails as the command word: a line's or span's first
	// (after VAR=value assignments), or after --, &&, ; or a pipe.
	cliUse    = regexp.MustCompile(`(?:^\s*(?:\$\s+)?(?:[A-Z_][A-Z0-9_]*=\S+\s+)*|--\s+|&&\s+|;\s+|\|\s+)openrails((?:[ \t]+[^\s|)&;]+)+)`)
	cliWord   = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	keyPath   = regexp.MustCompile(`^\.?[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)
	globalArg = map[string]bool{"--config": true, "-c": true, "--provider-write-mode": true}
)

// checkStale reports what standalone.md documents that does not exist: an
// `openrails` command, a key in the configuration reference's tables or a
// dotted path under a configuration key, and a key in the chart's tables.
// Methods need no rule here: the identifier references check every
// Type.Method the guide names against api/go.txt.
func checkStale(cov *Coverage, d *doc, add func(pos, class, format string, args ...any)) {
	pos := func(line int) string { return fmt.Sprintf("%s:%d", d.Path, line+1) }
	check := func(line int, words []string) {
		path, i := "", 0
		for ; i < len(words); i++ {
			w := words[i]
			if globalArg[w] {
				i++
				continue
			}
			if strings.HasPrefix(w, "-") { // subcommands come before a command's own flags
				break
			}
			if !cliWord.MatchString(w) {
				break
			}
			next := strings.TrimSpace(path + " " + w)
			if !cov.AllCommands[next] {
				if path == "" || hasChild(cov, path) && !hasArgs(words[i:]) {
					add(pos(line), cAbsent, "`openrails %s` is no command", next)
				}
				return
			}
			path = next
		}
	}
	inFence := map[int]bool{}
	for _, fe := range d.Fences {
		for l := fe.Open + 1; l < fe.Close; l++ {
			inFence[l] = true
			for _, m := range cliUse.FindAllStringSubmatch(d.Lines[l], -1) {
				check(l, strings.Fields(m[1]))
			}
		}
	}
	for _, s := range d.Spans {
		for _, m := range cliUse.FindAllStringSubmatch(s.Text, -1) {
			check(s.Line, strings.Fields(m[1]))
		}
	}

	top := map[string]bool{}
	for k := range cov.ConfigPaths {
		if !strings.Contains(k, ".") {
			top[k] = true
		}
	}
	checked := map[int]bool{}
	section := ""
	for l, line := range d.Lines {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
		}
		if inFence[l] || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		var known func(string) bool
		switch section {
		case "Configuration reference":
			known = cov.configKnown
		case "Kubernetes and Helm":
			known = func(k string) bool { return cov.HelmPaths[k] || underFreeForm(k) }
		default:
			continue
		}
		parent := ""
		for _, k := range codeSpans(cells[1]) {
			if i := strings.LastIndexByte(k, '.'); i > 0 {
				parent = k[:i]
			}
			if !keyPath.MatchString(k) {
				continue
			}
			if strings.HasPrefix(k, ".") {
				k = parent + k
			}
			if !known(k) {
				add(pos(l), cAbsent, "`%s` is no %s key", k, strings.ToLower(section))
			}
			checked[l] = true
		}
	}
	for _, s := range d.Spans {
		k := s.Text
		if i := strings.IndexByte(k, '.'); i > 0 && !checked[s.Line] && keyPath.MatchString(k) && top[normKey(k[:i])] && !cov.configKnown(k) {
			add(pos(s.Line), cAbsent, "`%s` is no configuration key", k)
		}
	}
}

// hasChild reports whether command path has subcommands.
func hasChild(cov *Coverage, path string) bool {
	for c := range cov.AllCommands {
		if strings.HasPrefix(c, path+" ") {
			return true
		}
	}
	return false
}

// hasArgs reports whether words start like a positional argument rather
// than a subcommand: a placeholder, a path or a value.
func hasArgs(words []string) bool {
	return len(words) > 0 && !cliWord.MatchString(words[0])
}

var spanRe = regexp.MustCompile("`([^`]+)`")

func codeSpans(cell string) []string {
	var out []string
	for _, m := range spanRe.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

// yamlPaths lists every key path of a YAML document, lists flattened.
func yamlPaths(raw []byte) []string {
	var v any
	if yaml.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, c := range t {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				out = append(out, p)
				walk(p, c)
			}
		case []any:
			for _, c := range t {
				walk(prefix, c)
			}
		}
	}
	walk("", v)
	return out
}

// buildCoverage reads what the reference pages must name from the tree.
func buildCoverage(root, goTxt string) (*Coverage, error) {
	cov := &Coverage{Methods: map[string][]string{}, AllCommands: map[string]bool{}, ConfigPaths: map[string]bool{}, HelmPaths: map[string]bool{}}
	seen := map[string]bool{}
	for _, line := range strings.Split(goTxt, "\n") {
		owner, name := "", ""
		if m := coveredMethod.FindStringSubmatch(line); m != nil {
			owner, name = m[1], m[2]
		} else if m := coveredFunc.FindStringSubmatch(line); m != nil {
			owner, name = "func", m[1]
		}
		if name != "" && !seen[owner+"."+name] {
			seen[owner+"."+name] = true
			cov.Methods[name] = append(cov.Methods[name], owner)
		}
	}

	keys := map[string]bool{}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "server", "internal", "hostconfig", "config.go"), nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "fileConfig" {
			return true
		}
		for _, fl := range ts.Type.(*ast.StructType).Fields.List {
			if fl.Tag != nil {
				tag, _ := strconv.Unquote(fl.Tag.Value)
				if k := reflect.StructTag(tag).Get("koanf"); k != "" {
					keys[k] = true
				}
			}
		}
		return false
	})
	if len(keys) == 0 {
		return nil, fmt.Errorf("server/internal/hostconfig: no fileConfig koanf keys")
	}
	example, err := os.ReadFile(filepath.Join(root, "config.example.yaml"))
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(example, &doc); err != nil {
		return nil, fmt.Errorf("config.example.yaml: %w", err)
	}
	var leaf func(prefix string, v any)
	leaf = func(prefix string, v any) {
		for _, c := range collapsed {
			if prefix == c {
				keys[prefix] = true
				return
			}
		}
		m, ok := v.(map[string]any)
		if !ok || len(m) == 0 {
			keys[prefix] = true
			return
		}
		for k, c := range m {
			leaf(prefix+"."+k, c)
		}
	}
	for k, v := range doc {
		leaf(k, v)
	}
	for k := range keys {
		cov.ConfigKeys = append(cov.ConfigKeys, k)
		cov.ConfigPaths[normPath(k)] = true
	}
	sort.Strings(cov.ConfigKeys)
	for _, k := range keyPaths(example) {
		cov.ConfigPaths[normPath(k)] = true
	}
	structs, maps, err := configStructPaths(root)
	if err != nil {
		return nil, err
	}
	for k := range structs {
		cov.ConfigPaths[k] = true
	}
	cov.ConfigMaps = maps

	values, err := os.ReadFile(filepath.Join(root, "deploy", "helm", "openrails", "values.yaml"))
	if err != nil {
		return nil, err
	}
	var top map[string]any
	if err := yaml.Unmarshal(values, &top); err != nil {
		return nil, fmt.Errorf("values.yaml: %w", err)
	}
	for k := range top {
		cov.HelmKeys = append(cov.HelmKeys, k)
	}
	sort.Strings(cov.HelmKeys)
	for _, k := range keyPaths(values) {
		cov.HelmPaths[k] = true
	}

	cmds, err := cliCommands(filepath.Join(root, "server", "cmd", "openrails"))
	if err != nil {
		return nil, err
	}
	for _, c := range cmds {
		cov.AllCommands[c] = true
		if !hiddenCommands[c] {
			cov.Commands = append(cov.Commands, c)
		}
	}
	return cov, nil
}

var keyLine = regexp.MustCompile(`^( *)(?:# ?( *))?((?:- )?)([a-z][a-z0-9_]*):(?:\s|$)`)

// keyPaths lists the key paths of a YAML file and their prefixes, reading
// commented-out keys too: an example file documents optional keys that way.
func keyPaths(raw []byte) []string {
	type level struct {
		indent int
		key    string
	}
	var stack []level
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		m := keyLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		indent := len(m[1]) + len(m[2]) + len(m[3])
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, level{indent, m[4]})
		parts := make([]string, len(stack))
		for i, l := range stack {
			parts[i] = l.key
		}
		out = append(out, strings.Join(parts, "."))
	}
	return out
}

// normKey compares configuration keys as the server's loader matches them:
// case-insensitive, underscores ignored.
func normKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(k, "_", ""))
}

// configStructPaths walks hostconfig's fileConfig and the internal/config
// types it reaches, as the loader decodes them (a koanf tag, else the field
// name), and returns every key path, normalized, and the paths of maps,
// whose keys are the operator's own.
func configStructPaths(root string) (paths, maps map[string]bool, err error) {
	types := map[string]map[string]*ast.StructType{"": {}, "billing": {}}
	for pkg, dir := range map[string]string{"": "server/internal/hostconfig", "billing": "internal/config"} {
		fset := token.NewFileSet()
		pkgs, err := parseGoDir(fset, filepath.Join(root, dir))
		if err != nil {
			return nil, nil, err
		}
		for _, files := range pkgs {
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					if ts, ok := n.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							types[pkg][ts.Name.Name] = st
						}
					}
					return true
				})
			}
		}
	}
	paths, maps = map[string]bool{}, map[string]bool{}
	var walk func(prefix, pkg string, st *ast.StructType, depth int)
	var typ func(path, pkg string, e ast.Expr, depth int)
	typ = func(path, pkg string, e ast.Expr, depth int) {
		switch t := e.(type) {
		case *ast.StarExpr:
			typ(path, pkg, t.X, depth)
		case *ast.ArrayType:
			typ(path, pkg, t.Elt, depth)
		case *ast.MapType:
			maps[path] = true
		case *ast.Ident:
			if st := types[pkg][t.Name]; st != nil {
				walk(path, pkg, st, depth+1)
			}
		case *ast.SelectorExpr:
			if x, ok := t.X.(*ast.Ident); ok && x.Name == "billing" {
				if st := types["billing"][t.Sel.Name]; st != nil {
					walk(path, "billing", st, depth+1)
				}
			}
		}
	}
	walk = func(prefix, pkg string, st *ast.StructType, depth int) {
		if depth > 8 {
			return
		}
		for _, f := range st.Fields.List {
			names := []string{}
			if f.Tag != nil {
				tag, _ := strconv.Unquote(f.Tag.Value)
				if k := reflect.StructTag(tag).Get("koanf"); k == "-" {
					continue
				} else if k != "" {
					names = []string{k}
				}
			}
			if len(f.Names) == 0 { // embedded: its fields are this level's
				typ(prefix, pkg, f.Type, depth)
				continue
			}
			if len(names) == 0 {
				for _, n := range f.Names {
					if n.IsExported() {
						names = append(names, n.Name)
					}
				}
			}
			for _, n := range names {
				p := normKey(n)
				if prefix != "" {
					p = prefix + "." + p
				}
				paths[p] = true
				typ(p, pkg, f.Type, depth)
			}
		}
	}
	file := types[""]["fileConfig"]
	if file == nil {
		return nil, nil, fmt.Errorf("server/internal/hostconfig: no fileConfig")
	}
	walk("", "", file, 0)
	return paths, maps, nil
}

func normPath(k string) string {
	parts := strings.Split(k, ".")
	for i, p := range parts {
		parts[i] = normKey(p)
	}
	return strings.Join(parts, ".")
}

// configKnown reports whether k is a configuration key path the server
// accepts, or one beneath a map or a free-form section.
func (cov *Coverage) configKnown(k string) bool {
	if underFreeForm(k) {
		return true
	}
	n := normPath(k)
	if cov.ConfigPaths[n] {
		return true
	}
	for m := range cov.ConfigMaps {
		if strings.HasPrefix(n, m+".") {
			return true
		}
	}
	return false
}
