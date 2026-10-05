package contract

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
)

// proseSkipped are the documents a generator writes, and history.
var proseSkipped = []string{RoutesDoc, CodesDoc, "sdk/billing-ui/CHANGELOG.md"}

// migrationGuide names what v1 removed in each table's first column; only the
// last column, what replaced it, is checked.
const migrationGuide = "docs/migrating-to-v1.md"

// foreignPaths are other services' /v1 paths the prose quotes.
var foreignPaths = []string{
	"/v1/account", "/v1/balance", // Stripe
}

var (
	prosePath   = regexp.MustCompile("(?:\\b(GET|POST|PUT|PATCH|DELETE) `?)?(?:/billing)?(/v1/[A-Za-z0-9_{}/:.*<>-]*)")
	proseGo     = regexp.MustCompile(`\b(openrails|billing|catalog)\.([A-Z]\w*)(?:\.([A-Z]\w*))?`)
	proseMethod = regexp.MustCompile(`\b[cC]lient\.([A-Z]\w*)\(`)
	proseField  = regexp.MustCompile(`(?:^|[^.\w])(Config|Deps|HTTPConfig|MerchantDeclaration|PSPConfig|CheckoutConfig)\.([A-Z]\w*)`)
	proseTable  = regexp.MustCompile(`\bbilling\.([a-z][a-z0-9_]*)\b(\.?)`)
	proseCode   = regexp.MustCompile("\\b([1-5]\\d\\d) `?([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\\b")
	prosePerm   = regexp.MustCompile(`\b((?:merchant|root)(?::[a-z0-9-]+){2,})`)
	fileSuffix  = []string{"yaml", "yml", "json", "jsonl", "ts", "tsx", "js", "go", "md", "sql", "css", "example"}
)

// TestDocsNameWhatExists keeps the prose on the surface it describes: every
// route, Go identifier, table, permission and error code (with its status) a
// document names is in the route catalog, api/go.txt, api/schema.txt or the
// error-code registry. A rename or a removal fails here until its documents
// follow.
func TestDocsNameWhatExists(t *testing.T) {
	root := filepath.Join("..", "..")
	goAPI := readGoAPI(t, filepath.Join(root, "api", "go.txt"))
	tables := readSchemaNames(t, filepath.Join(root, "api", "schema.txt"))

	var docs []string
	for _, pattern := range []string{"README.md", ".claude/CLAUDE.md", "compatibility/README.md", "sdk/billing-ui/README.md", "sdk/billing-ui/docs/*.md", "web/admin/README.md"} {
		found, err := filepath.Glob(filepath.Join(root, pattern))
		require.NoError(t, err)
		docs = append(docs, found...)
	}
	require.NoError(t, filepath.WalkDir(filepath.Join(root, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			docs = append(docs, path)
		}
		return err
	}))

	for _, doc := range docs {
		rel, _ := filepath.Rel(root, doc)
		rel = filepath.ToSlash(rel)
		if slices.Contains(proseSkipped, rel) {
			continue
		}
		body, err := os.ReadFile(doc)
		require.NoError(t, err)
		for n, line := range strings.Split(string(body), "\n") {
			at := rel + ":" + strconv.Itoa(n+1)
			if rel == migrationGuide {
				row, ok := strings.CutSuffix(strings.TrimSpace(line), "|")
				if !ok {
					continue
				}
				line = row[strings.LastIndex(row, "|")+1:]
			}
			for _, m := range prosePath.FindAllStringSubmatch(line, -1) {
				if why := routeNamed(m[1], m[2]); why != "" {
					t.Errorf("%s: %s: %s", at, strings.TrimSpace(m[0]), why)
				}
			}
			for _, m := range proseGo.FindAllStringSubmatch(line, -1) {
				pkg, name, member := m[1], m[2], m[3]
				switch members, ok := goAPI[pkg+"."+name]; {
				case !ok:
					t.Errorf("%s: %s.%s is not in api/go.txt", at, pkg, name)
				case member != "" && len(members) > 0 && !members[member]:
					t.Errorf("%s: %s is not in api/go.txt", at, m[0])
				}
			}
			for _, m := range proseMethod.FindAllStringSubmatch(line, -1) {
				if !goAPI["openrails.Client"][m[1]] {
					t.Errorf("%s: Client.%s is not in api/go.txt", at, m[1])
				}
			}
			for _, m := range proseField.FindAllStringSubmatch(line, -1) {
				if !goAPI["openrails."+m[1]][m[2]] {
					t.Errorf("%s: %s is not in api/go.txt", at, m[0])
				}
			}
			for _, m := range proseCode.FindAllStringSubmatch(line, -1) {
				switch code, ok := billing.LookupErrorCode(m[2]); {
				case !ok:
					t.Errorf("%s: %s is not a registered error code", at, m[2])
				case strconv.Itoa(code.Status) != m[1]:
					t.Errorf("%s: %s answers %d, not %s", at, m[2], code.Status, m[1])
				}
			}
			for _, m := range prosePerm.FindAllStringSubmatch(line, -1) {
				if !goAPI["permissions"][m[1]] {
					t.Errorf("%s: %s is not a permission in api/go.txt", at, m[1])
				}
			}
			for _, m := range proseTable.FindAllStringSubmatch(line, -1) {
				// billing.yaml is a file and billing.example.com a host.
				if m[2] != "" || slices.Contains(fileSuffix, m[1]) || tables[m[1]] {
					continue
				}
				t.Errorf("%s: %s is not in api/schema.txt", at, strings.TrimSuffix(m[0], "."))
			}
		}
	}
}

// routeNamed is why the catalog has no such route, or "".
func routeNamed(method, path string) string {
	path = strings.TrimRight(path, ".:/")
	if slices.Contains(foreignPaths, path) {
		return ""
	}
	prefix, wildcard := strings.CutSuffix(path, "/*")
	if !wildcard {
		prefix, wildcard = strings.CutSuffix(path, "*")
	}
	want := strings.Split(prefix, "/")
	pathKnown := false
	for _, r := range routes.Catalog() {
		have := strings.Split(r.Path, "/")
		if len(have) < len(want) || (!wildcard && method != "" && len(have) != len(want)) {
			continue
		}
		match := true
		for i, segment := range want {
			placeholder := strings.HasPrefix(have[i], "{")
			switch {
			case segment == have[i]:
			case wildcard && i == len(want)-1 && strings.HasPrefix(have[i], segment):
			// A concrete id in an example stands for the parameter; a
			// parameter is spelled as the catalog spells it.
			case placeholder && !strings.ContainsAny(segment, "{}<>:"):
			default:
				match = false
			}
			if !match {
				break
			}
		}
		if !match {
			continue
		}
		if method == "" || wildcard || r.Method == method {
			return ""
		}
		pathKnown = len(have) == len(want)
	}
	if pathKnown {
		return "the route catalog has this path, not this method"
	}
	return "not in the route catalog"
}

// readGoAPI maps each listed identifier ("billing.Price") to its fields and
// methods, and "permissions" to the permission names.
func readGoAPI(t *testing.T, file string) map[string]map[string]bool {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	decl := regexp.MustCompile(`^pkg openrails(?:/(\w+))?, (?:const|var|func|type) (\w+)`)
	member := regexp.MustCompile(`^pkg openrails(?:/(\w+))?, (?:type (\w+) (?:struct|interface), |method \(\*?(\w+)(?:\[[^\]]*\])?\) )(\w+)`)
	permission := regexp.MustCompile(`^pkg openrails/billing, const \w+ untyped string = "((?:merchant|root):[^"]+)"`)
	out := map[string]map[string]bool{"permissions": {}}
	for _, line := range strings.Split(string(body), "\n") {
		if m := permission.FindStringSubmatch(line); m != nil {
			out["permissions"][m[1]] = true
		}
		if m := member.FindStringSubmatch(line); m != nil {
			owner := pkgName(m[1]) + "." + m[2] + m[3]
			if out[owner] == nil {
				out[owner] = map[string]bool{}
			}
			out[owner][m[4]] = true
		} else if m := decl.FindStringSubmatch(line); m != nil {
			if owner := pkgName(m[1]) + "." + m[2]; out[owner] == nil {
				out[owner] = map[string]bool{}
			}
		}
	}
	require.NotEmpty(t, out["openrails.Client"])
	return out
}

func pkgName(sub string) string {
	if sub == "" {
		return "openrails"
	}
	return sub
}

// readSchemaNames is every table and function api/schema.txt lists.
func readSchemaNames(t *testing.T, file string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:table|function|procedure) billing\.(\w+)`).FindAllStringSubmatch(string(body), -1) {
		out[m[1]] = true
	}
	require.NotEmpty(t, out)
	return out
}
