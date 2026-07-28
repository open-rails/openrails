//go:build cgo

package sqlaudit

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Exemption is one reviewed allowlist entry: `rule subject # rationale` under a
// `## PERMANENT` or `## DEBT` section. Format is shared with tensorhub's gate.
type Exemption struct {
	Class     string // PERMANENT (bounded by design) or DEBT (a tracked bug)
	Rule      string
	Subject   string // query name
	Rationale string // mandatory
	Line      int
}

type Allowlist struct {
	entries map[string]Exemption // "subject:rule"
	used    map[string]bool
}

// LoadAllowlist parses the allowlist, rejecting anything unreviewable: an entry
// outside a class section, a missing rationale, or a duplicate.
func LoadAllowlist(path string) (*Allowlist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	a := &Allowlist{entries: map[string]Exemption{}, used: map[string]bool{}}
	class := ""
	sc := bufio.NewScanner(f)
	for ln := 1; sc.Scan(); ln++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "##") {
			switch c := strings.TrimSpace(strings.TrimPrefix(line, "##")); c {
			case "PERMANENT", "DEBT":
				class = c
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		rule, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s:%d: want `rule subject # rationale`, got %q", path, ln, line)
		}
		subject, rationale, hasNote := strings.Cut(rest, "#")
		subject = strings.TrimSpace(subject)
		rationale = strings.TrimSpace(rationale)
		switch {
		case class == "":
			return nil, fmt.Errorf("%s:%d: entry before any `## PERMANENT` / `## DEBT` section", path, ln)
		case subject == "" || strings.Contains(subject, " "):
			return nil, fmt.Errorf("%s:%d: want one subject between rule and `#`, got %q", path, ln, rest)
		case !hasNote || rationale == "":
			return nil, fmt.Errorf("%s:%d: %s %s has no rationale — every exemption must say why", path, ln, rule, subject)
		}
		key := subject + ":" + rule
		if prev, dup := a.entries[key]; dup {
			return nil, fmt.Errorf("%s:%d: duplicate of line %d (%s %s)", path, ln, prev.Line, rule, subject)
		}
		a.entries[key] = Exemption{Class: class, Rule: rule, Subject: subject, Rationale: rationale, Line: ln}
	}
	return a, sc.Err()
}

func (a *Allowlist) exempt(f Finding) (Exemption, bool) {
	key := f.Query + ":" + f.Rule
	e, ok := a.entries[key]
	if ok {
		a.used[key] = true
	}
	return e, ok
}

// Stale returns entries nothing tripped — the ratchet: fix a query and its
// exemption must go with it.
func (a *Allowlist) Stale() []Exemption {
	var out []Exemption
	for k, e := range a.entries {
		if !a.used[k] {
			out = append(out, e)
		}
	}
	return out
}
