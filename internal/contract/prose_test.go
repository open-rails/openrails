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
	proseHost   = regexp.MustCompile(`https?://[^/\s]+`)
	prosePath   = regexp.MustCompile("(?:\\b(GET|POST|PUT|PATCH|DELETE) `?|(?:^|[^\\w/.:-]))(?:/billing)?(/v1/[A-Za-z0-9_{}/:.*<>-]*)")
	proseGo     = regexp.MustCompile(`\b(openrails|billing|catalog|server)\.([A-Z]\w*)(?:\.([A-Z]\w*))?`)
	proseMethod = regexp.MustCompile(`\b[cC]lient\.([A-Z]\w*)`)
	// A bare Go name in a code span that reads as a Client operation.
	proseVerb  = regexp.MustCompile("`((?:Get|List|Create|Update|Delete|Set|Ensure|Apply|Archive|Cancel|Resume|Refund|Record|Capture|Release|Extend|Admit|Open|Preview|Refresh|Resolve|Revoke|Confirm|Check|Has|Import|Export|Provision|Rename|Declare|Retry|Mark|Void|Ask|Query|Generate|Verify|Acknowledge|Report|Change|Retire|Complete)[A-Z]\\w*)(?:\\([^`]*\\))?`")
	proseField = regexp.MustCompile(`(?:^|[^.\w])(Config|Deps|HTTPConfig|MerchantDeclaration|PSPConfig|CheckoutConfig)\.([A-Z]\w*)`)
	proseTable = regexp.MustCompile(`\bbilling\.([a-z][a-z0-9_]*)\b(\.?)`)
	proseCode  = regexp.MustCompile("\\b([1-5]\\d\\d) `?([a-z][a-z0-9]*(?:_[a-z0-9]+)+)\\b")
	prosePerm  = regexp.MustCompile(`\b((?:merchant|root)(?::[a-z0-9-]+){2,})`)
	proseTest  = regexp.MustCompile(`\bTest[A-Z]\w*`)
	// A constraint, index or trigger named in a code span.
	proseObject = regexp.MustCompile("`((?:chk|idx|ix|uq|trg|fk)_[a-z0-9_]+|[a-z][a-z0-9_]*_(?:fk|pkey|check|idx))`")
	// A private tracker id means nothing to a reader of the public docs.
	proseIssue = regexp.MustCompile("(?:^|[^&\\w`])([a-z]{0,3}#\\d{2,4}\\b|SEC-\\d+)")
	fileSuffix = []string{"yaml", "yml", "json", "jsonl", "ts", "tsx", "js", "go", "md", "sql", "css", "example"}
)

// TestDocsNameWhatExists keeps the prose on the surface it describes: every
// route, Go identifier, table, permission and error code (with its status) a
// document names is in the route catalog, api/go.txt, api/schema.txt or the
// error-code registry. A rename or a removal fails here until its documents
// follow. A test, constraint or index a document names exists, and no
// document cites a tracker issue.
func TestDocsNameWhatExists(t *testing.T) {
	root := filepath.Join("..", "..")
	goAPI := readGoAPI(t, filepath.Join(root, "api", "go.txt"))
	tables, objects := readSchemaNames(t, filepath.Join(root, "api", "schema.txt"))

	tests := readTestNames(t, root)

	for _, rel := range proseDocs(t, root) {
		if slices.Contains(proseSkipped, rel) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, rel))
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
			// A URL's path is checked; another service's mount (/api/v1) is not.
			for _, m := range prosePath.FindAllStringSubmatch(proseHost.ReplaceAllString(line, " "), -1) {
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
			// billing-ui's documents name TypeScript, not Go.
			if !strings.HasPrefix(rel, "sdk/") {
				for _, m := range proseVerb.FindAllStringSubmatch(line, -1) {
					if !goAPI["names"][m[1]] {
						t.Errorf("%s: %s is not in api/go.txt", at, m[1])
					}
				}
			}
			for _, m := range proseField.FindAllStringSubmatch(line, -1) {
				if !goAPI["openrails."+m[1]][m[2]] {
					t.Errorf("%s: %s is not in api/go.txt", at, m[0])
				}
			}
			for _, m := range proseIssue.FindAllStringSubmatch(line, -1) {
				t.Errorf("%s: %s cites a tracker issue", at, m[1])
			}
			for _, name := range proseTest.FindAllString(line, -1) {
				// TestMode is the Config field; TestReplicas* names a family.
				if name != "TestMode" && !slices.ContainsFunc(tests, func(test string) bool { return strings.HasPrefix(test, name) }) {
					t.Errorf("%s: no test is named %s", at, name)
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
			for _, m := range proseObject.FindAllStringSubmatch(line, -1) {
				if !objects[m[1]] {
					t.Errorf("%s: %s is not in api/schema.txt", at, m[1])
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

// TestREADMEEmbeddedExample keeps the README's install example the program
// in examples/embedded, which compiles: its catalog, its newBilling and its
// billingAuth.
func TestREADMEEmbeddedExample(t *testing.T) {
	root := filepath.Join("..", "..")
	read := func(rel string) string {
		body, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		return string(body)
	}
	readme, program := read("README.md"), read("examples/embedded/main.go")
	_, catalog, ok := strings.Cut(readme, "```yaml\n")
	require.True(t, ok)
	catalog, _, _ = strings.Cut(catalog, "```")
	require.Equal(t, read("examples/embedded/catalog.yaml"), catalog, "the README's catalog.yaml")

	_, body, ok := strings.Cut(readme, "func newBilling(")
	require.True(t, ok)
	body, _, _ = strings.Cut(body, "\n}\n")
	require.Contains(t, program, "func newBilling("+body+"\n}\n", "the README's newBilling")

	// The README's roles, its AuthKit configuration (but its production keys)
	// and its mount are the program's.
	_, roles, ok := strings.Cut(readme, "rbac := authkit.NewRoles()\n")
	require.True(t, ok)
	roles, _, _ = strings.Cut(roles, "```")
	for _, line := range strings.Split(strings.TrimSpace(roles), "\n") {
		require.Contains(t, program, "\t"+strings.TrimRight(line, " ")+"\n", "the README's role registration")
	}
	_, config, ok := strings.Cut(readme, "ak, err := authkit.New(ctx, authkit.Config{\n")
	require.True(t, ok)
	config, _, _ = strings.Cut(config, "}, authkit.Deps{")
	for _, line := range strings.Split(strings.TrimSpace(config), "\n") {
		field, _, _ := strings.Cut(strings.TrimSpace(line), ":")
		if field == "Keys" || field == "Token" {
			continue // the program signs with development keys, as its own issuer
		}
		require.Contains(t, strings.Join(strings.Fields(program), " "), strings.Join(strings.Fields(line), " "), "the README's AuthKit configuration")
	}
	_, mount, ok := strings.Cut(readme, "\t// Billing. Public, customer (/me) and webhook routes are always mounted.\n")
	require.True(t, ok)
	mount, _, _ = strings.Cut(mount, "\n\t})\n")
	require.Contains(t, program, mount, "the README's mount")
	require.NotContains(t, readme+program, "billingAuth", "AuthKit is the Auth; no adapter")

	// The README's contacts are AuthKit, as the program's are.
	_, contacts, ok := strings.Cut(readme, "#### Customer contact info")
	require.True(t, ok)
	_, contacts, ok = strings.Cut(contacts, "\tContacts: ")
	require.True(t, ok)
	contacts, _, _ = strings.Cut(contacts, "\n")
	require.Contains(t, program, "\t\tContacts: "+contacts+"\n", "the README's Deps.Contacts")
}

// proseDocs lists the documents, relative to the repository root.
func proseDocs(t *testing.T, root string) []string {
	t.Helper()
	var docs []string
	for _, pattern := range []string{"README.md", ".claude/CLAUDE.md", "compatibility/README.md", "sdk/billing-ui/*.md", "sdk/billing-ui/docs/*.md", "web/admin/README.md"} {
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
	for i, doc := range docs {
		rel, err := filepath.Rel(root, doc)
		require.NoError(t, err)
		docs[i] = filepath.ToSlash(rel)
	}
	return docs
}

var (
	proseLink    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	proseHeading = regexp.MustCompile("(?m)^#{1,6} +(.+?) *$")
	slugDropped  = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
	fencedBlock  = regexp.MustCompile("(?s)```.*?```")
)

// TestDocsLinksResolve keeps every relative link on a file that exists and,
// when it names one, a heading of that file.
func TestDocsLinksResolve(t *testing.T) {
	root := filepath.Join("..", "..")
	anchors := map[string]map[string]bool{}
	anchorsOf := func(rel string) map[string]bool {
		if anchors[rel] == nil {
			anchors[rel] = map[string]bool{}
			body, err := os.ReadFile(filepath.Join(root, rel))
			require.NoError(t, err)
			for _, m := range proseHeading.FindAllStringSubmatch(fencedBlock.ReplaceAllString(string(body), ""), -1) {
				text := proseLink.ReplaceAllString(m[1], "")
				slug := strings.ReplaceAll(slugDropped.ReplaceAllString(strings.ToLower(text), ""), " ", "-")
				anchors[rel][slug] = true
			}
		}
		return anchors[rel]
	}
	for _, rel := range proseDocs(t, root) {
		body, err := os.ReadFile(filepath.Join(root, rel))
		require.NoError(t, err)
		for _, m := range proseLink.FindAllStringSubmatch(fencedBlock.ReplaceAllString(string(body), ""), -1) {
			target, fragment, _ := strings.Cut(m[1], "#")
			if strings.Contains(target, ":") { // another site
				continue
			}
			file := rel
			if target != "" {
				file = filepath.ToSlash(filepath.Join(filepath.Dir(rel), target))
				if _, err := os.Stat(filepath.Join(root, file)); err != nil {
					t.Errorf("%s: link to %s: no such file", rel, m[1])
					continue
				}
			}
			if fragment != "" && strings.HasSuffix(file, ".md") && !anchorsOf(file)[fragment] {
				t.Errorf("%s: link to %s: no such heading", rel, m[1])
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
// methods, "permissions" to the permission names, and "names" to every
// exported name the list mentions.
func readGoAPI(t *testing.T, file string) map[string]map[string]bool {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	decl := regexp.MustCompile(`^pkg openrails(?:/(\w+))?, (?:const|var|func|type) (\w+)`)
	member := regexp.MustCompile(`^pkg openrails(?:/(\w+))?, (?:type (\w+) (?:struct|interface), |method \(\*?(\w+)(?:\[[^\]]*\])?\) )(\w+)`)
	permission := regexp.MustCompile(`^pkg openrails/billing, const \w+ untyped string = "((?:merchant|root):[^"]+)"`)
	word := regexp.MustCompile(`\b[A-Z]\w+`)
	out := map[string]map[string]bool{"permissions": {}, "names": {}}
	for _, line := range strings.Split(string(body), "\n") {
		for _, name := range word.FindAllString(line, -1) {
			out["names"][name] = true
		}
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

// readTestNames is every Go test function in the repository.
func readTestNames(t *testing.T, root string) []string {
	t.Helper()
	declared := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range declared.FindAllSubmatch(body, -1) {
				out = append(out, string(m[1]))
			}
		}
		return nil
	}))
	return out
}

func pkgName(sub string) string {
	if sub == "" {
		return "openrails"
	}
	return sub
}

// readSchemaNames is every table and function api/schema.txt lists, and
// every name it lists at all: columns, constraints, indexes and triggers too.
func readSchemaNames(t *testing.T, file string) (tables, objects map[string]bool) {
	t.Helper()
	body, err := os.ReadFile(file)
	require.NoError(t, err)
	tables, objects = map[string]bool{}, map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:table|function|procedure) billing\.(\w+)`).FindAllStringSubmatch(string(body), -1) {
		tables[m[1]], objects[m[1]] = true, true
	}
	for _, m := range regexp.MustCompile(`(?m)^  (?:column|constraint|index CREATE (?:UNIQUE )?INDEX|trigger CREATE (?:CONSTRAINT )?TRIGGER) "?(\w+)`).FindAllStringSubmatch(string(body), -1) {
		objects[m[1]] = true
	}
	require.NotEmpty(t, tables)
	return tables, objects
}
