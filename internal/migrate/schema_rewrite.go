package migrate

import (
	"fmt"
	"strings"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
)

// Relocate schema references in our embedded SQL/PLpgSQL migrations, preserving
// domain strings and comments. The lexer distinguishes quoted values from identifiers; dollar-quoted
// AS/DO bodies contain code and are scanned recursively. No cgo is required.
func rewriteMigrationsSchema(migrations []migratekit.Migration, schema string) ([]migratekit.Migration, error) {
	if schema == "" || schema == config.DefaultSchema {
		return migrations, nil
	}
	out := make([]migratekit.Migration, len(migrations))
	for i, mig := range migrations {
		content, err := relocateSchemaSQL(mig.Content, schema)
		if err != nil {
			return nil, fmt.Errorf("relocate migration %s: %w", mig.Name, err)
		}
		mig.Content = content
		out[i] = mig
	}
	return out, nil
}

func relocateSchemaSQL(sql, schema string) (string, error) {
	tokens, err := schemaTokens(sql)
	if err != nil {
		return "", err
	}
	word := func(i int) string {
		if i < 0 || i >= len(tokens) {
			return ""
		}
		return strings.ToLower(sql[tokens[i].start:tokens[i].end])
	}
	var out strings.Builder
	end := 0
	pathPhase := 0 // SET search_path: 1=TO/=, 2=name, 3=comma.
	for i, token := range tokens {
		raw := sql[token.start:token.end]
		replacement := raw
		searchPath := pathPhase == 2
		switch {
		case word(i) == "search_path" && word(i-1) == "set":
			pathPhase = 1
		case pathPhase == 1 && (word(i) == "to" || raw == "="):
			pathPhase = 2
		case pathPhase == 2 && (token.kind == schemaIdentifier || token.kind == schemaString):
			pathPhase = 3
		case pathPhase == 3 && raw == ",":
			pathPhase = 2
		default:
			pathPhase = 0
			searchPath = false
		}
		if token.kind == schemaIdentifier && (word(i) == config.DefaultSchema || raw == `"`+config.DefaultSchema+`"`) {
			bareSchema := word(i-1) == "schema" || (word(i-1) == "exists" && word(i-2) == "not" && word(i-3) == "if" && word(i-4) == "schema")
			if word(i+1) == "." || bareSchema || searchPath {
				replacement = schema
			}
		}
		if token.kind == schemaString {
			switch {
			case strings.HasPrefix(raw, "$") && (word(i-1) == "as" || word(i-1) == "do"):
				delimiterEnd := strings.IndexByte(raw[1:], '$') + 2
				delimiter := raw[:delimiterEnd]
				body, err := relocateSchemaSQL(raw[delimiterEnd:len(raw)-delimiterEnd], schema)
				if err != nil {
					return "", fmt.Errorf("SQL function body: %w", err)
				}
				replacement = delimiter + body + delimiter
			case searchPath && raw == "'"+config.DefaultSchema+"'":
				replacement = "'" + schema + "'"
			case word(i+1) == "::" && (word(i+2) == "regclass" || word(i+2) == "regnamespace" || word(i+2) == "regtype" || word(i+2) == "regprocedure"):
				if strings.HasPrefix(raw, "'"+config.DefaultSchema+".") || raw == "'"+config.DefaultSchema+"'" {
					replacement = "'" + schema + raw[len(config.DefaultSchema)+1:]
				}
			}
		}
		out.WriteString(sql[end:token.start])
		out.WriteString(replacement)
		end = token.end
	}
	out.WriteString(sql[end:])
	return out.String(), nil
}
