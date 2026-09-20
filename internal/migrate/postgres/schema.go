package postgresmigrations

import (
	"regexp"
	"strings"

	"github.com/open-rails/openrails/config"
)

var (
	canonicalWord = regexp.MustCompile(`\b` + regexp.QuoteMeta(config.CanonicalSchema) + `\b`)
	schemaDDL     = regexp.MustCompile(`(?i)\bSCHEMA\s+(?:(?:IF\s+NOT\s+EXISTS|IF\s+EXISTS)\s+)?openrails\b`)
	schemaPath    = regexp.MustCompile(`(?i)\bSET\s+search_path\s+(?:TO|=)\s+(?:'[^']*'|"[^"]*"|[a-z_][a-z0-9_]*)(?:\s*,\s*(?:'[^']*'|"[^"]*"|[a-z_][a-z0-9_]*))*`)
)

// RewriteSchema relocates authored SQL identifiers and function search paths.
// Bare data literals such as rebill_driver='openrails' retain their meaning.
// The caller validates schema as a SQL identifier before executing the result.
func RewriteSchema(sql, schema string) string {
	if schema == "" {
		schema = config.DefaultSchema
	}
	if schema == config.CanonicalSchema {
		return sql
	}
	sql = strings.ReplaceAll(sql, config.CanonicalSchema+".", schema+".")
	sql = schemaDDL.ReplaceAllStringFunc(sql, func(clause string) string { return canonicalWord.ReplaceAllString(clause, schema) })
	return schemaPath.ReplaceAllStringFunc(sql, func(clause string) string { return canonicalWord.ReplaceAllString(clause, schema) })
}
