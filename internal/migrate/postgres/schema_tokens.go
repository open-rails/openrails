package postgresmigrations

import (
	"fmt"
	"strings"
)

type schemaToken struct{ start, end, kind int }

const (
	schemaPunctuation = iota
	schemaIdentifier
	schemaString
)

// Tokenize only; do not parse or reformat SQL. Comments remain in the original
// byte gaps, and literals remain whole even when they contain SQL-looking text.
func schemaTokens(sql string) ([]schemaToken, error) {
	var tokens []schemaToken
	for i := 0; i < len(sql); {
		start, kind := i, schemaPunctuation
		switch {
		case strings.ContainsRune(" \t\r\n\f", rune(sql[i])):
			i++
			continue
		case strings.HasPrefix(sql[i:], "--"):
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		case strings.HasPrefix(sql[i:], "/*"):
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				switch {
				case strings.HasPrefix(sql[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(sql[i:], "*/"):
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("unterminated SQL comment")
			}
			continue
		case sql[i] == '\'' || sql[i] == '"' || ((sql[i] == 'E' || sql[i] == 'e') && i+1 < len(sql) && sql[i+1] == '\''):
			escaped := sql[i] == 'E' || sql[i] == 'e'
			if escaped {
				i++
			}
			quote := sql[i]
			kind = schemaString
			if quote == '"' {
				kind = schemaIdentifier
			}
			i++
			closed := false
			for i < len(sql) {
				if escaped && sql[i] == '\\' {
					i += 2
					continue
				}
				if sql[i] == quote {
					i++
					if i < len(sql) && sql[i] == quote {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated SQL quoted token")
			}
		case sql[i] == '$':
			j := i + 1
			for j < len(sql) && schemaWord(sql[j]) {
				j++
			}
			if j < len(sql) && sql[j] == '$' && (j == i+1 || sql[i+1] < '0' || sql[i+1] > '9') {
				delimiter := sql[i : j+1]
				closing := strings.Index(sql[j+1:], delimiter)
				if closing < 0 {
					return nil, fmt.Errorf("unterminated SQL function body")
				}
				i = j + 1 + closing + len(delimiter)
				kind = schemaString
			} else {
				i++
			}
		case schemaWord(sql[i]):
			i++
			kind = schemaIdentifier
			for i < len(sql) && (schemaWord(sql[i]) || sql[i] == '$') {
				i++
			}
		case strings.HasPrefix(sql[i:], "::"):
			i += 2
		default:
			i++
		}
		tokens = append(tokens, schemaToken{start, i, kind})
	}
	return tokens, nil
}

func schemaWord(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= 128
}
