// docscheck keeps the documentation true to the code: guide snippets match
// their `guide:NAME` regions in examples/, YAML regions parse strictly,
// code spans name routes, Go identifiers, permissions, error codes, event
// types and schema objects that exist, and relative links and anchors
// resolve. -write rewrites tagged fences from their regions; -inventory
// lists the Go identifiers docs/guide.md uses.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/configdocument"
	"github.com/open-rails/openrails/internal/http/routes"
)

func main() {
	write := flag.Bool("write", false, "rewrite tagged fences from their examples/ regions")
	inventory := flag.Bool("inventory", false, "list the Go identifiers docs/guide.md references")
	root := flag.String("root", ".", "repository checkout or export to check")
	flag.Parse()
	if err := run(*root, *write, *inventory); err != nil {
		fmt.Fprintln(os.Stderr, "docscheck:", err)
		os.Exit(1)
	}
}

func run(root string, write, inventory bool) error {
	files, err := listFiles(root)
	if err != nil {
		return err
	}
	idx, err := realIndex(root)
	if err != nil {
		return err
	}
	rep, err := Check(Tree{Root: root, Files: files}, idx, write)
	if err != nil {
		return err
	}
	if inventory {
		names := make([]string, 0, len(rep.Uses))
		for n := range rep.Uses {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%4d %s\n", rep.Uses[n], n)
		}
		return nil
	}
	for _, f := range rep.Findings {
		fmt.Printf("%s: %s: %s\n", f.Pos, f.Class, f.Detail)
	}
	line, ok := rep.Summary()
	fmt.Println(line)
	if !ok {
		os.Exit(1)
	}
	return nil
}

// listFiles is the checkout's files git does not ignore when root is its
// top level, else every file under root.
func listFiles(root string) ([]string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if top, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output(); err == nil && strings.TrimSpace(string(top)) == abs {
		out, err := exec.Command("git", "-C", abs, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
		if err != nil {
			return nil, err
		}
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				if _, err := os.Lstat(filepath.Join(abs, f)); err == nil {
					files = append(files, f)
				}
			}
		}
		return files, nil
	}
	var files []string
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(abs, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, err
}

var schemaObject = regexp.MustCompile(`^(?:table|view|function|sequence) billing\.([a-z_][a-z0-9_]*)`)

func realIndex(root string) (*Index, error) {
	idx := &Index{Routes: map[string]bool{}, Codes: map[string]int{}, Permissions: map[string]bool{}, Events: map[string]bool{}, Schema: map[string]map[string]bool{}}
	for _, r := range routes.Catalog() {
		p := wildcard.ReplaceAllString(r.Path, "{}")
		idx.Routes[r.Method+" "+p] = true
		idx.Paths = append(idx.Paths, strings.Split(strings.TrimPrefix(p, "/"), "/"))
	}
	for _, c := range billing.ErrorCodes() {
		idx.Codes[c.Code] = c.Status
	}
	for _, p := range billing.Permissions() {
		idx.Permissions[string(p)] = true
	}
	goTxt, err := os.ReadFile(filepath.Join(root, "api", "go.txt"))
	if err != nil {
		return nil, err
	}
	idx.Go = parseGoTxt(string(goTxt))
	idx.JSONFields = map[string]bool{}
	for _, m := range regexp.MustCompile(`json:"([a-z0-9_]+)`).FindAllStringSubmatch(string(goTxt), -1) {
		idx.JSONFields[m[1]] = true
	}
	// AuthKit's Go and React names, and billing-ui's exports, are named too.
	if out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/open-rails/authkit").Output(); err == nil {
		ak := strings.TrimSpace(string(out))
		if err := idx.Go.addGoSource(ak, filepath.Join(ak, "auth"), filepath.Join(ak, "userinfo"), filepath.Join(ak, "internal", "config")); err != nil {
			return nil, err
		}
		idx.Go.addTSExports(readAll(filepath.Join(ak, "auth-ui", "src"), "index.ts")...)
	} else {
		fmt.Fprintln(os.Stderr, "docscheck: AuthKit's source is not in the module cache; its names count as dead")
	}
	idx.Go.addTSExports(readAll(filepath.Join(root, "sdk", "billing-ui", "src"), "index.ts")...)
	if idx.Coverage, err = buildCoverage(root, string(goTxt)); err != nil {
		return nil, err
	}
	event := regexp.MustCompile(`HostEventType = "([^"]+)"`)
	for _, m := range event.FindAllStringSubmatch(string(goTxt), -1) {
		idx.Events[m[1]] = true
	}
	schema, err := os.Open(filepath.Join(root, "api", "schema.txt"))
	if err != nil {
		return nil, err
	}
	defer schema.Close()
	var cols map[string]bool
	sc := bufio.NewScanner(schema)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := schemaObject.FindStringSubmatch(line); m != nil {
			cols = map[string]bool{}
			idx.Schema[m[1]] = cols
		} else if c, ok := strings.CutPrefix(line, "  column "); ok && cols != nil {
			cols[strings.Fields(c)[0]] = true
		} else if line != "" && line[0] != ' ' {
			cols = nil
		}
	}
	idx.ParseYAML = func(raw []byte) error {
		_, err := configdocument.YAMLToJSON(raw, 1<<20)
		return err
	}
	idx.ParseFile = func(name string, raw []byte) error {
		base := filepath.Base(name)
		switch {
		case strings.HasPrefix(base, "catalog"):
			_, err := catalog.ParseApplicationYAML(raw)
			return err
		case strings.HasPrefix(base, "merchant"):
			// A standalone manifest (top-level merchants) is the server
			// module's to parse; only its YAML is checked here.
			doc, err := configdocument.YAMLToJSON(raw, 1<<20)
			if err != nil {
				return err
			}
			var top map[string]json.RawMessage
			if json.Unmarshal(doc, &top) == nil && top["merchants"] != nil {
				return nil
			}
			// W4 renames this ParseMerchantDocument.
			_, err = openrails.ParseMerchantDeclaration(raw)
			return err
		}
		return nil
	}
	return idx, sc.Err()
}

// readAll reads every file named name under dir.
func readAll(dir, name string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			if b, err := os.ReadFile(p); err == nil {
				out = append(out, string(b))
			}
		}
		return nil
	})
	return out
}
