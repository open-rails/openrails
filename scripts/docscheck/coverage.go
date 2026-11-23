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
	cov := &Coverage{Methods: map[string][]string{}}
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
	}
	sort.Strings(cov.ConfigKeys)

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

	cmds, err := cliCommands(filepath.Join(root, "server", "cmd", "openrails"))
	if err != nil {
		return nil, err
	}
	for _, c := range cmds {
		if !hiddenCommands[c] {
			cov.Commands = append(cov.Commands, c)
		}
	}
	return cov, nil
}
