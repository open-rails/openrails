package copilot

import "strings"

// renderTable formats rows as a compact, token-efficient TOON-style table —
// header + pipe-separated cells, one row per line — rather than pretty JSON.
// Used both for the context pack's live catalog summary (content-first: the
// model reads this before any doctrine text) and for tool results (AXI:
// "token-efficient output"). An explicit empty-state line replaces a bare
// header-only table so the model never has to infer "0 rows" from absence.
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
