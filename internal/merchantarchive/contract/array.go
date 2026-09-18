package contract

import "strings"

// PostgreSQL's one-dimensional text-array spelling. Reject dimensions, nested
// arrays and NULL members: the retained rails/window keys never use them.
func safeTextArray(v string) bool {
	if len(v) < 2 || v[0] != '{' || v[len(v)-1] != '}' {
		return false
	}
	v = v[1 : len(v)-1]
	if v == "" {
		return true
	}
	for len(v) > 0 {
		var item strings.Builder
		quoted := v[0] == '"'
		if quoted {
			v = v[1:]
		}
		closed := !quoted
		for len(v) > 0 {
			b := v[0]
			if b == '\\' {
				if len(v) < 2 {
					return false
				}
				item.WriteByte(v[1])
				v = v[2:]
				continue
			}
			if quoted && b == '"' {
				v = v[1:]
				closed = true
				break
			}
			if !quoted && b == ',' {
				break
			}
			if !quoted && (b == '{' || b == '}' || b == '"' || b == ' ' || b == '\t' || b == '\n') {
				return false
			}
			item.WriteByte(b)
			v = v[1:]
		}
		if !closed || (!quoted && (item.Len() == 0 || strings.EqualFold(item.String(), "NULL"))) || !safeText(item.String()) {
			return false
		}
		if v == "" {
			return true
		}
		if v[0] != ',' || len(v) == 1 {
			return false
		}
		v = v[1:]
	}
	return false
}
