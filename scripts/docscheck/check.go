package main

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Index is what references are checked against.
type Index struct {
	Routes      map[string]bool // "METHOD /path" with {} wildcards
	Paths       [][]string      // every route path, by segment
	Codes       map[string]int  // error code to status
	Permissions map[string]bool
	Events      map[string]bool
	JSONFields  map[string]bool            // every json tag name of api/go.txt
	Schema      map[string]map[string]bool // object to its columns
	Go          *goIndex
	Coverage    *Coverage // nil: no coverage rules
	// ParseYAML parses a YAML region strictly; ParseFile parses a whole
	// examples file by its kind (catalog, merchant) and is nil-safe.
	ParseYAML func([]byte) error
	ParseFile func(name string, raw []byte) error
}

// Tree is the files checked: Root on disk, Files every tracked path.
type Tree struct {
	Root  string
	Files []string
}

// Finding is one problem, by class.
type Finding struct {
	Pos, Class, Detail string
}

// Report is a run's findings and, with -write, the fences rewritten.
type Report struct {
	Findings  []Finding
	Rewritten int
	Uses      map[string]int // Go identifiers docs/guide.md references
}

// Classes, in summary order.
const (
	cRoute, cIdent, cPerm, cCode, cEvent, cSchema = "route", "identifier", "permission", "code", "event", "schema"
	cLink                                         = "link"
	cStale, cUntagged, cUnknown, cUnused, cRegion = "stale-snippet", "untagged-fence", "unknown-region", "unused-region", "region"
	cYAML                                         = "yaml"
)

// allow are code spans that look like Go identifiers but name no Go API of
// OpenRails or AuthKit: River and pgx settings, JavaScript and HTTP names,
// CCBill's webhook event types and a CloudNativePG kind.
var allow = map[string]bool{
	"PollOnly": true, "QueryExecModeCacheDescribe": true, "DefaultQueryExecMode": true, "ContextTimeoutEnabled": true,
	"RescueStuckJobsAfter": true, "JobTimeout": true, "StopAndCancel": true, "ParseConfig": true,
	"BigInt": true, "ETag": true, "ScheduledBackup": true,
	"NewSaleSuccess": true, "NewSaleFailure": true, "RenewalSuccess": true, "RenewalFailure": true, "UpgradeSuccess": true,
	"UpgradeFailure": true, "BillingDateChange": true, "CustomerDataUpdate": true, "UserReactivation": true,
}

var (
	routeSpan  = regexp.MustCompile(`^(?:(GET|POST|PUT|PATCH|DELETE|HEAD) +)?(\S*/v1(?:/\S*)?)$`)
	ourPrefix  = regexp.MustCompile(`^(?:\{\w+\}|https?://[^/]+)?(?:/billing)?$`)
	provider   = regexp.MustCompile(`(?:stripe|nmi|ccbill|networkmerchants|basistheory|hyperswitch)\.com`)
	permSpan   = regexp.MustCompile(`^(merchant|root):[a-z-]+:[a-z]+$`)
	codeSpan   = regexp.MustCompile(`^([45]\d\d) +([a-z][a-z0-9_]*)$`)
	snakeSpan  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	statusTail = regexp.MustCompile(`(?:^|[^\w.])([45]\d\d) $`)
	dotted     = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)
	schemaSpan = regexp.MustCompile(`^(billing|billing_river)\.([a-z_][a-z0-9_]*)(?:\.([a-z_][a-z0-9_]*))?(?:\(\))?$`)
	identSpan  = regexp.MustCompile(`^[A-Za-z_]\w*(\.[A-Za-z_]\w*)*$`)
	humps      = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)
	wildcard   = regexp.MustCompile(`\{[^}]*\}`)
	githubRe   = regexp.MustCompile(`^https://github\.com/open-rails/openrails/(?:blob|tree)/master/([^#?]*)(#.*)?$`)
	schemeRe   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

var goPkgs = map[string]bool{"openrails": true, "billing": true, "catalog": true, "server": true, "openrailstest": true, "nmimock": true, "stripemock": true, "console": true}

var riverTables = map[string]bool{"river_job": true, "river_leader": true, "river_queue": true, "river_client": true, "river_client_queue": true, "river_migration": true}

// yamlSourced are the pages whose YAML fences must come from examples/;
// elsewhere an untagged YAML fence (a configuration reference) need only parse.
var yamlSourced = map[string]bool{"docs/guide.md": true, "docs/embedded.md": true, "docs/client.md": true}

// fenceText is a fence's body without its indentation.
func fenceText(d *doc, fe fence) string {
	lines := make([]string, 0, fe.Close-fe.Open)
	for _, l := range d.Lines[fe.Open+1 : fe.Close] {
		lines = append(lines, strings.TrimPrefix(l, strings.Repeat(" ", fe.Indent)))
	}
	return strings.Join(lines, "\n")
}

// linksOnly names the pages only the link check covers: the generated
// references, the project's agent notes, and the architecture docs, which
// name internal packages, files and tables rather than the public surface.
func linksOnly(p string) bool {
	return p == "docs/api/routes.md" || p == "docs/api/error-codes.md" ||
		strings.HasPrefix(p, ".claude/") || strings.HasPrefix(p, "docs/architecture/")
}

func excluded(p string) bool {
	return strings.HasPrefix(p, "scripts/testdata/") || strings.HasPrefix(p, "scripts/docscheck/testdata/")
}

// Check runs every rule over the tree; with write it rewrites tagged fences.
func Check(t Tree, idx *Index, write bool) (*Report, error) {
	rep := &Report{Uses: map[string]int{}}
	add := func(pos, class, format string, args ...any) {
		rep.Findings = append(rep.Findings, Finding{pos, class, fmt.Sprintf(format, args...)})
	}
	exists := map[string]bool{".": true}
	for _, f := range t.Files {
		exists[f] = true
		for d := path.Dir(f); d != "."; d = path.Dir(d) {
			exists[d] = true
		}
	}
	read := func(p string) (string, error) {
		b, err := os.ReadFile(filepath.Join(t.Root, filepath.FromSlash(p)))
		return string(b), err
	}

	regions := map[string]*region{}
	yamlFiles := map[string]bool{}
	for _, f := range t.Files {
		if !strings.HasPrefix(f, "examples/") {
			continue
		}
		text, err := read(f)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(text, "guide:") {
			continue
		}
		rs, errs := parseRegions(f, text)
		rep.Findings = append(rep.Findings, errs...)
		for _, r := range rs {
			if prev := regions[r.Name]; prev != nil {
				add(fmt.Sprintf("%s:%d", r.File, r.Line), cRegion, "region %s is also defined at %s:%d", r.Name, prev.File, prev.Line)
				continue
			}
			regions[r.Name] = r
			if ext := path.Ext(f); ext == ".yaml" || ext == ".yml" {
				yamlFiles[f] = true
				if idx.ParseYAML != nil {
					if err := idx.ParseYAML([]byte(strings.Join(r.Body, "\n") + "\n")); err != nil {
						add(fmt.Sprintf("%s:%d", r.File, r.Line), cYAML, "region %s: %v", r.Name, err)
					}
				}
			}
		}
	}
	for f := range yamlFiles {
		if idx.ParseFile == nil {
			continue
		}
		text, _ := read(f)
		if err := idx.ParseFile(f, []byte(text)); err != nil {
			add(f, cYAML, "%v", err)
		}
	}

	docs := map[string]*doc{}
	var mds []string
	for _, f := range t.Files {
		if strings.HasSuffix(f, ".md") && !excluded(f) {
			mds = append(mds, f)
		}
	}
	load := func(p string) *doc {
		if d, ok := docs[p]; ok {
			return d
		}
		text, err := read(p)
		if err != nil {
			docs[p] = nil
			return nil
		}
		docs[p] = parseDoc(p, text)
		return docs[p]
	}

	used := map[string]bool{}
	for _, f := range mds {
		d := load(f)
		if d == nil {
			continue
		}
		pos := func(line int) string { return fmt.Sprintf("%s:%d", f, line+1) }
		hand := !linksOnly(f)
		changed := false
		for i := len(d.Fences) - 1; i >= 0; i-- {
			fe := d.Fences[i]
			if fe.Source == "" {
				if !hand {
					continue
				}
				goFence, yamlFence := fe.Lang == "go" || fe.Lang == "golang", fe.Lang == "yaml" || fe.Lang == "yml"
				switch {
				case goFence && (strings.HasPrefix(f, "docs/") || f == "README.md"), yamlFence && yamlSourced[f]:
					add(pos(fe.Open), cUntagged, "%s fence has no guide: source", fe.Lang)
				case yamlFence && idx.ParseYAML != nil:
					if err := idx.ParseYAML([]byte(fenceText(d, fe) + "\n")); err != nil {
						add(pos(fe.Open), cYAML, "untagged YAML fence: %v", err)
					}
				}
				continue
			}
			used[fe.Source] = true
			r := regions[fe.Source]
			if r == nil {
				add(pos(fe.Open), cUnknown, "no region guide:%s in examples/", fe.Source)
				continue
			}
			body := d.Lines[fe.Open+1 : fe.Close]
			indent := strings.Repeat(" ", fe.Indent)
			want := make([]string, len(r.Body))
			for k, l := range r.Body {
				if l != "" {
					want[k] = indent + l
				}
			}
			if strings.Join(body, "\n") == strings.Join(want, "\n") {
				continue
			}
			if !write {
				add(pos(fe.Open), cStale, "guide:%s differs from %s:%d; run docscheck -write", fe.Source, r.File, r.Line)
				continue
			}
			d.Lines = append(d.Lines[:fe.Open+1], append(want, d.Lines[fe.Close:]...)...)
			changed = true
			rep.Rewritten++
		}
		if changed {
			text := strings.Join(d.Lines, "\n")
			if err := os.WriteFile(filepath.Join(t.Root, filepath.FromSlash(f)), []byte(text), 0o644); err != nil {
				return nil, err
			}
			d = parseDoc(f, text)
			docs[f] = d
		}

		if hand {
			for _, s := range d.Spans {
				checkSpan(idx, f, s, func(class, format string, args ...any) { add(pos(s.Line), class, format, args...) }, rep.Uses)
			}
		}

		for _, l := range d.Links {
			target := l.Target
			if m := githubRe.FindStringSubmatch(target); m != nil {
				target = "/" + m[1] + m[2]
			} else if target == "" || schemeRe.MatchString(target) || strings.HasPrefix(target, "//") {
				continue
			}
			p, anchor, _ := strings.Cut(target, "#")
			if q := strings.IndexByte(p, '?'); q >= 0 {
				p = p[:q]
			}
			if u, err := url.PathUnescape(p); err == nil {
				p = u
			}
			resolved := f
			if p != "" {
				if strings.HasPrefix(p, "/") {
					resolved = path.Clean(strings.TrimPrefix(p, "/"))
				} else {
					resolved = path.Clean(path.Join(path.Dir(f), p))
				}
				if !exists[resolved] {
					add(pos(l.Line), cLink, "%s: no such file", l.Target)
					continue
				}
			}
			if anchor == "" || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			td := load(resolved)
			if td == nil || !td.Anchors[strings.ToLower(anchor)] && !td.Anchors[anchor] {
				add(pos(l.Line), cLink, "%s: no heading #%s in %s", l.Target, anchor, resolved)
			}
		}
	}
	if idx.Coverage != nil {
		checkCoverage(idx.Coverage, load, add)
	}
	for name, r := range regions {
		if !used[name] {
			add(fmt.Sprintf("%s:%d", r.File, r.Line), cUnused, "region guide:%s is not used by any fence", name)
		}
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool { return lessPos(rep.Findings[i].Pos, rep.Findings[j].Pos) })
	return rep, nil
}

func lessPos(a, b string) bool {
	af, al, _ := strings.Cut(a, ":")
	bf, bl, _ := strings.Cut(b, ":")
	if af != bf {
		return af < bf
	}
	an, _ := strconv.Atoi(al)
	bn, _ := strconv.Atoi(bl)
	return an < bn
}

// checkSpan classifies one inline code span and checks it.
func checkSpan(idx *Index, file string, s span, bad func(class, format string, args ...any), uses map[string]int) {
	text := strings.TrimSpace(s.Text)
	if m := routeSpan.FindStringSubmatch(text); m != nil && !strings.ContainsAny(text, "|[*…") && !strings.Contains(text, "...") {
		at := strings.Index(m[2], "/v1")
		if !ourPrefix.MatchString(m[2][:at]) || provider.MatchString(m[2][:at]) {
			return // another service's path: AuthKit's, a provider's
		}
		p := m[2][at:]
		if q := strings.IndexByte(p, '?'); q >= 0 {
			p = p[:q]
		}
		if p != "/v1" {
			p = strings.TrimSuffix(p, "/")
		}
		if !idx.route(m[1], p) {
			bad(cRoute, "%s names no route", text)
		}
		return
	}
	if permSpan.MatchString(text) {
		if !idx.Permissions[text] {
			bad(cPerm, "%s is not in billing.Permissions()", text)
		}
		return
	}
	status, code := "", ""
	if m := codeSpan.FindStringSubmatch(text); m != nil {
		status, code = m[1], m[2]
	} else if snakeSpan.MatchString(text) {
		if m := statusTail.FindStringSubmatch(s.Before); m != nil {
			status, code = m[1], text
		}
	}
	if code != "" {
		want, ok := idx.Codes[code]
		switch {
		case !ok && !strings.Contains(code, "_"):
			// a single word after a status is prose or another protocol's
			// error (SCIM's uniqueness), unless it is ours
		case !ok:
			bad(cCode, "%s %s is not a registered error code", status, code)
		case strconv.Itoa(want) != status:
			bad(cCode, "%s is %d in the registry, not %s", code, want, status)
		}
		return
	}
	if m := schemaSpan.FindStringSubmatch(text); m != nil {
		if m[1] == "billing_river" {
			if !riverTables[m[2]] {
				bad(cSchema, "%s is not a River table", text)
			}
			return
		}
		cols, ok := idx.Schema[m[2]]
		switch {
		case !ok:
			bad(cSchema, "%s is not in api/schema.txt", text)
		case m[3] != "" && !cols[m[3]]:
			bad(cSchema, "%s: no column %s in api/schema.txt", text, m[3])
		}
		return
	}
	if dotted.MatchString(text) {
		family, name, _ := strings.Cut(text, ".")
		// order.pay_url is a JSON path, not an event type
		if idx.eventFamily(family) && !strings.Contains(name, ".") && !idx.JSONFields[name] && !idx.Events[text] {
			bad(cEvent, "%s is not a billing.HostEventType", text)
		}
		return
	}
	if strings.HasPrefix(file, "web/billing-ui/") {
		return
	}
	checkIdent(idx, file, text, bad, uses)
}

func (idx *Index) route(method, p string) bool {
	p = wildcard.ReplaceAllString(p, "{}")
	if method != "" {
		if idx.Routes[method+" "+p] {
			return true
		}
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for key := range idx.Routes {
			m, rp, _ := strings.Cut(key, " ")
			if m == method && segMatch(segs, strings.Split(strings.TrimPrefix(rp, "/"), "/"), false) {
				return true
			}
		}
		return false
	}
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for _, rp := range idx.Paths {
		if segMatch(segs, rp, true) {
			return true
		}
	}
	return false
}

// segMatch matches a doc path against a route path: a route wildcard takes
// any segment, a doc wildcard only a route wildcard.
func segMatch(doc, route []string, prefix bool) bool {
	if len(doc) > len(route) || !prefix && len(doc) != len(route) {
		return false
	}
	for i, s := range doc {
		if route[i] != "{}" && route[i] != s {
			return false
		}
	}
	return true
}

func (idx *Index) eventFamily(f string) bool {
	for e := range idx.Events {
		if strings.HasPrefix(e, f+".") {
			return true
		}
	}
	return false
}

func checkIdent(idx *Index, file, text string, bad func(class, format string, args ...any), uses map[string]int) {
	text = strings.TrimLeft(text, "*&")
	if i := strings.IndexAny(text, "({"); i > 0 && strings.ContainsAny(text[len(text)-1:], ")}") {
		text = text[:i]
	}
	for {
		i := strings.IndexByte(text, '[')
		j := strings.IndexByte(text, ']')
		if i < 0 || j < i {
			break
		}
		text = text[:i] + text[j+1:]
	}
	if !identSpan.MatchString(text) || idx.Go == nil {
		return
	}
	parts := strings.Split(text, ".")
	x := idx.Go
	use := func(name string) {
		if file == "docs/guide.md" {
			uses[name]++
		}
	}
	head := parts[0]
	if head[0] >= 'a' && head[0] <= 'z' {
		if !goPkgs[head] || len(parts) < 2 || !humps.MatchString(parts[1]) {
			return
		}
		if !x.names[head][parts[1]] {
			bad(cIdent, "%s: %s has no %s in api/go.txt", text, head, parts[1])
			return
		}
		if len(parts) == 2 {
			use(head + "." + parts[1])
			return
		}
		t := x.types[head][parts[1]]
		if t == nil {
			bad(cIdent, "%s: %s.%s is not a type", text, head, parts[1])
			return
		}
		if p, ok := x.chain(t, parts[2:]); !ok {
			bad(cIdent, "%s: no %s in api/go.txt", text, p)
		} else {
			use(p)
		}
		return
	}
	if allow[head] || len(head) == 1 || strings.HasPrefix(head, "Test") && len(parts) == 1 || len(head) >= 32 {
		return // version placeholders (X.Y.Z), Go test names, base58 keys
	}
	for _, p := range parts[1:] {
		if p[0] < 'A' || p[0] > 'Z' {
			return // a file name (Taskfile.yaml) or a JSON path
		}
	}
	if len(parts) == 1 {
		if !twoHumps(head) {
			return
		}
		if !x.all[head] {
			bad(cIdent, "%s is no name in api/go.txt", text)
			return
		}
		use(head)
		return
	}
	cands := x.byName[head]
	if len(cands) == 0 {
		bad(cIdent, "%s: %s is no type in api/go.txt", text, head)
		return
	}
	var miss string
	for _, t := range cands {
		p, ok := x.chain(t, parts[1:])
		if ok {
			use(p)
			return
		}
		if miss == "" {
			miss = p
		}
	}
	bad(cIdent, "%s: no %s in api/go.txt", text, miss)
}

// twoHumps: an exported CamelCase name with a lower-case letter and at
// least two capitals, e.g. CreateOrder or PSPConfig, not NMI or Allowed.
func twoHumps(s string) bool {
	if !humps.MatchString(s) {
		return false
	}
	upper, lower := 0, 0
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			upper++
		case r >= 'a' && r <= 'z':
			lower++
		}
	}
	return upper >= 2 && lower > 0
}

// Summary is the closing line.
func (r *Report) Summary() (string, bool) {
	n := map[string]int{}
	for _, f := range r.Findings {
		n[f.Class]++
	}
	dead := n[cRoute] + n[cIdent] + n[cPerm] + n[cCode] + n[cEvent] + n[cSchema]
	line := fmt.Sprintf("docscheck: dead references %d (routes %d, identifiers %d, permissions %d, codes %d, events %d, schema %d); broken links %d; snippets: stale %d, untagged %d, unknown %d, unused %d, malformed %d; yaml %d; coverage %d; absent %d",
		dead, n[cRoute], n[cIdent], n[cPerm], n[cCode], n[cEvent], n[cSchema], n[cLink], n[cStale], n[cUntagged], n[cUnknown], n[cUnused], n[cRegion], n[cYAML], n[cCoverage], n[cAbsent])
	if r.Rewritten > 0 {
		line += fmt.Sprintf("; rewrote %d fences", r.Rewritten)
	}
	return line, len(r.Findings) == 0
}
