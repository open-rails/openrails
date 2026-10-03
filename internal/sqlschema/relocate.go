package sqlschema

import (
	"fmt"
	"strings"
)

var regTypes = map[string]bool{"regclass": true, "regnamespace": true, "regtype": true, "regproc": true, "regprocedure": true}

var regFuncs = map[string]bool{"to_regclass": true, "to_regnamespace": true, "to_regtype": true, "to_regproc": true, "to_regprocedure": true}

// relocate moves schema references from one schema to another: qualifiers and
// quoted-identifier qualifiers in code, the name after SCHEMA, SET search_path
// values and reg* literals. Other literals and comments are data and stay;
// dollar-quoted function bodies (after AS or DO) are relocated recursively.
func relocate(sql, from, to string) (string, error) {
	toks, err := tokenize(sql)
	if err != nil {
		return "", err
	}
	word := func(i int) string {
		if i < 0 || i >= len(toks) {
			return ""
		}
		return strings.ToLower(sql[toks[i].start:toks[i].end])
	}
	quoted := `"` + from + `"`
	var out strings.Builder
	end := 0
	pathPhase := 0 // SET search_path: 1=TO/=, 2=name, 3=comma.
	for i, t := range toks {
		raw := sql[t.start:t.end]
		replacement := raw
		searchPath := pathPhase == 2
		switch {
		case word(i) == "search_path" && (word(i-1) == "set" || (word(i-2) == "set" && (word(i-1) == "local" || word(i-1) == "session"))):
			pathPhase = 1
		case pathPhase == 1 && (word(i) == "to" || raw == "="):
			pathPhase = 2
		case pathPhase == 2 && (t.kind == identifier || t.kind == literal):
			pathPhase = 3
		case pathPhase == 3 && raw == ",":
			pathPhase = 2
		default:
			pathPhase = 0
			searchPath = false
		}
		switch t.kind {
		case identifier:
			if word(i) != from && raw != quoted {
				break
			}
			schemaName := word(i-1) == "schema" ||
				(word(i-1) == "exists" && word(i-2) == "if" && word(i-3) == "schema") ||
				(word(i-1) == "exists" && word(i-2) == "not" && word(i-3) == "if" && word(i-4) == "schema")
			if word(i+1) == "." || schemaName || searchPath {
				replacement = to
				if raw == quoted {
					replacement = `"` + to + `"`
				}
			}
		case literal:
			switch {
			case strings.HasPrefix(raw, "$"):
				if word(i-1) != "as" && word(i-1) != "do" {
					break
				}
				delimiter := raw[:strings.IndexByte(raw[1:], '$')+2]
				body, err := relocate(raw[len(delimiter):len(raw)-len(delimiter)], from, to)
				if err != nil {
					return "", fmt.Errorf("function body: %w", err)
				}
				replacement = delimiter + body + delimiter
			case searchPath:
				replacement = relocateList(raw, from, to)
			case (word(i+1) == "::" && regTypes[word(i+2)]) || (word(i-1) == "(" && regFuncs[word(i-2)] && word(i+1) == ")"):
				if raw == "'"+from+"'" || strings.HasPrefix(raw, "'"+from+".") {
					replacement = "'" + to + raw[len(from)+1:]
				}
			}
		}
		out.WriteString(sql[end:t.start])
		out.WriteString(replacement)
		end = t.end
	}
	out.WriteString(sql[end:])
	return out.String(), nil
}

// relocateList rewrites from in a quoted search_path value such as 'a, b'.
func relocateList(raw, from, to string) string {
	if len(raw) < 2 || raw[0] != '\'' {
		return raw
	}
	parts := strings.Split(raw[1:len(raw)-1], ",")
	for i, p := range parts {
		if strings.TrimSpace(p) == from {
			parts[i] = strings.Replace(p, from, to, 1)
		}
	}
	return "'" + strings.Join(parts, ",") + "'"
}
