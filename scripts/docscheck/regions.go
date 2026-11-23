package main

import (
	"fmt"
	"regexp"
	"strings"
)

// region is one `guide:NAME` region of an examples file.
type region struct {
	Name, File string
	Line       int // 1-based line of the opening marker
	Body       []string
}

var markerRe = regexp.MustCompile(`^\s*(?://|#)\s*guide:([a-z0-9][a-z0-9-]*)\s*$`)

// parseRegions reads the regions of one file. Markers never reach a body;
// guide:hide … guide:show drops lines from every enclosing region.
func parseRegions(path, text string) ([]*region, []Finding) {
	var out, stack []*region
	var errs []Finding
	bad := func(line int, format string, args ...any) {
		errs = append(errs, Finding{fmt.Sprintf("%s:%d", path, line), cRegion, fmt.Sprintf(format, args...)})
	}
	hidden := 0
	for i, line := range strings.Split(text, "\n") {
		m := markerRe.FindStringSubmatch(line)
		if m == nil {
			if hidden == 0 {
				for _, r := range stack {
					r.Body = append(r.Body, line)
				}
			}
			continue
		}
		switch m[1] {
		case "end":
			if len(stack) == 0 {
				bad(i+1, "guide:end with no open region")
				continue
			}
			r := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			r.Body = dedent(r.Body)
			out = append(out, r)
		case "hide":
			hidden++
		case "show":
			if hidden == 0 {
				bad(i+1, "guide:show with no guide:hide")
				continue
			}
			hidden--
		default:
			stack = append(stack, &region{Name: m[1], File: path, Line: i + 1})
		}
	}
	for _, r := range stack {
		bad(r.Line, "region %s is never closed", r.Name)
	}
	if hidden > 0 {
		bad(1, "guide:hide is never shown")
	}
	return out, errs
}

// dedent trims blank edge lines and the common leading whitespace.
func dedent(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	prefix := ""
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		ws := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if i == 0 {
			prefix = ws
		}
		for !strings.HasPrefix(ws, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out[i] = strings.TrimRight(strings.TrimPrefix(l, prefix), " \t")
	}
	return out
}
