package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// fence is a fenced code block: Open and Close are 0-based line numbers.
type fence struct {
	Open, Close int
	Indent      int
	Lang        string
	Source      string // the guide:NAME token, if any
}

// span is an inline code span: its text, the 0-based line it starts on and
// the block text before it on that line's block.
type span struct {
	Text   string
	Line   int
	Before string
}

type link struct {
	Target string
	Line   int
}

// doc is a parsed markdown file.
type doc struct {
	Path    string
	Lines   []string
	Fences  []fence
	Spans   []span
	Links   []link
	Anchors map[string]bool
}

var (
	fenceRe    = regexp.MustCompile("^(\\s*)(`{3,}|~{3,})(.*)$")
	atxRe      = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	setextRe   = regexp.MustCompile(`^\s{0,3}(=+|-+)\s*$`)
	refDefRe   = regexp.MustCompile(`^\s{0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	inlineRe   = regexp.MustCompile(`\]\(\s*(?:<([^>]*)>|([^()\s]*(?:\([^()\s]*\)[^()\s]*)*))(?:\s+"[^"]*")?\s*\)`)
	hrefRe     = regexp.MustCompile(`<(?:a|img)\s[^>]*(?:href|src)="([^"]+)"`)
	anchorDef  = regexp.MustCompile(`<a\s[^>]*(?:id|name)="([^"]+)"`)
	headLinkRe = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	tagRe      = regexp.MustCompile(`<[^>]+>`)
)

func parseDoc(path, text string) *doc {
	d := &doc{Path: path, Lines: strings.Split(text, "\n"), Anchors: map[string]bool{}}
	prose := make([]string, len(d.Lines)) // lines outside fences; fence lines blank
	seen := map[string]int{}
	addHeading := func(h string) {
		s := slug(h)
		if n, dup := seen[s]; dup {
			seen[s] = n + 1
			s = s + "-" + strconv.Itoa(n+1)
		} else {
			seen[s] = 0
		}
		d.Anchors[s] = true
	}
	for i := 0; i < len(d.Lines); i++ {
		line := d.Lines[i]
		if m := fenceRe.FindStringSubmatch(line); m != nil && !(m[2][0] == '`' && strings.Contains(m[3], "`")) {
			f := fence{Open: i, Indent: len(m[1]), Close: len(d.Lines)}
			info := strings.Fields(m[3])
			if len(info) > 0 {
				f.Lang = strings.ToLower(info[0])
			}
			for _, tok := range info {
				if strings.HasPrefix(tok, "guide:") {
					f.Source = strings.TrimPrefix(tok, "guide:")
				}
			}
			marker := m[2]
			for j := i + 1; j < len(d.Lines); j++ {
				t := strings.TrimSpace(d.Lines[j])
				if strings.HasPrefix(t, marker[:1]) && strings.Trim(t, marker[:1]) == "" && len(t) >= len(marker) {
					f.Close = j
					break
				}
			}
			d.Fences = append(d.Fences, f)
			i = f.Close
			continue
		}
		prose[i] = line
		if m := atxRe.FindStringSubmatch(line); m != nil {
			addHeading(m[2])
		} else if i > 0 && setextRe.MatchString(line) && prose[i-1] != "" && !strings.HasPrefix(strings.TrimSpace(prose[i-1]), "|") &&
			!strings.HasPrefix(strings.TrimSpace(prose[i-1]), "- ") && atxRe.FindString(prose[i-1]) == "" && strings.TrimSpace(line)[0] == '=' {
			addHeading(prose[i-1])
		}
		for _, m := range anchorDef.FindAllStringSubmatch(line, -1) {
			d.Anchors[m[1]] = true
		}
	}
	// Blocks: runs of non-blank prose lines.
	for start := 0; start < len(prose); {
		if strings.TrimSpace(prose[start]) == "" {
			start++
			continue
		}
		end := start
		for end < len(prose) && strings.TrimSpace(prose[end]) != "" {
			end++
		}
		d.block(prose[start:end], start)
		start = end
	}
	return d
}

// block finds the code spans and links of one block of prose lines.
func (d *doc) block(lines []string, first int) {
	text := strings.Join(lines, "\n")
	lineAt := func(off int) int { return first + strings.Count(text[:off], "\n") }
	masked := []byte(text)
	for i := 0; i < len(text); {
		if text[i] == '\\' {
			i += 2
			continue
		}
		if text[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(text) && text[i+n] == '`' {
			n++
		}
		closeAt := -1
		for j := i + n; j < len(text); {
			if text[j] != '`' {
				j++
				continue
			}
			k := 0
			for j+k < len(text) && text[j+k] == '`' {
				k++
			}
			if k == n {
				closeAt = j
				break
			}
			j += k
		}
		if closeAt < 0 {
			i += n
			continue
		}
		body := strings.ReplaceAll(text[i+n:closeAt], "\n", " ")
		if len(body) > 1 && body[0] == ' ' && body[len(body)-1] == ' ' && strings.TrimSpace(body) != "" {
			body = body[1 : len(body)-1]
		}
		lineStart := strings.LastIndexByte(text[:i], '\n') + 1
		d.Spans = append(d.Spans, span{Text: body, Line: lineAt(i), Before: text[lineStart:i]})
		for k := i; k < closeAt+n; k++ {
			if masked[k] != '\n' {
				masked[k] = ' '
			}
		}
		i = closeAt + n
	}
	m := string(masked)
	for _, loc := range inlineRe.FindAllStringSubmatchIndex(m, -1) {
		target := ""
		if loc[2] >= 0 {
			target = m[loc[2]:loc[3]]
		} else if loc[4] >= 0 {
			target = m[loc[4]:loc[5]]
		}
		d.Links = append(d.Links, link{Target: target, Line: lineAt(loc[0])})
	}
	for _, loc := range hrefRe.FindAllStringSubmatchIndex(m, -1) {
		d.Links = append(d.Links, link{Target: m[loc[2]:loc[3]], Line: lineAt(loc[0])})
	}
	for i, l := range strings.Split(m, "\n") {
		if r := refDefRe.FindStringSubmatch(l); r != nil {
			d.Links = append(d.Links, link{Target: r[1], Line: first + i})
		}
	}
}

// slug is GitHub's heading anchor.
func slug(h string) string {
	h = headLinkRe.ReplaceAllString(h, "$1")
	h = tagRe.ReplaceAllString(h, "")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}
