package copilot

import "strings"

// renderTable formats rows as a compact pipe-separated table rather than JSON,
// for the context pack and tool results. An explicit emptyState line replaces
// a header-only table so the model never infers "0 rows" from absence.
func renderTable(headers []string, rows [][]string, emptyState string) string {
	if len(rows) == 0 {
		return emptyState
	}
	var b strings.Builder
	b.WriteString(strings.Join(headers, " | "))
	b.WriteByte('\n')
	for _, row := range rows {
		b.WriteString(strings.Join(row, " | "))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
