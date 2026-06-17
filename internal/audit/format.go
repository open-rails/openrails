package audit

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/fatih/color"
)

// Formatter handles output formatting for audit results
type Formatter interface {
	Format(w io.Writer, findings []Finding, summary Summary) error
}

// GetFormatter returns the appropriate formatter for the given format string
func GetFormatter(format string) Formatter {
	switch strings.ToLower(format) {
	case "json":
		return &JSONFormatter{}
	case "csv":
		return &CSVFormatter{}
	default:
		return &TableFormatter{}
	}
}

// TableFormatter outputs results as a colored table
type TableFormatter struct{}

func (f *TableFormatter) Format(w io.Writer, findings []Finding, summary Summary) error {
	return f.FormatDetails(w, findings, summary)
}

// FormatSummary prints the terminal-friendly audit summary. Individual findings
// belong in the detailed audit log, not stdout.
func (f *TableFormatter) FormatSummary(w io.Writer, findings []Finding, summary Summary, detailsPath string) error {
	// Define colors
	critical := color.New(color.FgRed, color.Bold)
	high := color.New(color.FgRed)
	medium := color.New(color.FgYellow)
	low := color.New(color.FgCyan)
	header := color.New(color.FgWhite, color.Bold)

	// Print header
	header.Fprintln(w)
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	header.Fprintln(w, "                    BILLING CONSISTENCY AUDIT")
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)

	if len(findings) == 0 {
		color.New(color.FgGreen, color.Bold).Fprintln(w, "✓ No consistency issues found!")
		return nil
	}

	fmt.Fprintf(w, "Total findings:     %d\n", summary.TotalFindings)
	fmt.Fprintf(w, "Auto-fixable:       %d\n", summary.AutoFixable)
	fmt.Fprintf(w, "Manual review:      %d\n", summary.ManualReviewOnly)
	if detailsPath != "" {
		fmt.Fprintf(w, "Detailed log:       %s\n", detailsPath)
	}

	fmt.Fprintln(w, "\nBy Severity:")
	if v := summary.BySeverity[SeverityCritical]; v > 0 {
		critical.Fprintf(w, "  CRITICAL: %d\n", v)
	}
	if v := summary.BySeverity[SeverityHigh]; v > 0 {
		high.Fprintf(w, "  HIGH:     %d\n", v)
	}
	if v := summary.BySeverity[SeverityMedium]; v > 0 {
		medium.Fprintf(w, "  MEDIUM:   %d\n", v)
	}
	if v := summary.BySeverity[SeverityLow]; v > 0 {
		low.Fprintf(w, "  LOW:      %d\n", v)
	}

	fmt.Fprintln(w, "\nBy Category:")
	for _, cat := range sortedKeys(summary.ByCategory) {
		fmt.Fprintf(w, "  %-25s %d\n", cat+":", summary.ByCategory[cat])
	}

	fmt.Fprintln(w, "\nBy Check:")
	for _, row := range checkCounts(findings) {
		fmt.Fprintf(w, "  %-8s %-45s %d\n", row.id, row.name, row.count)
	}
	fmt.Fprintln(w)

	return nil
}

// FormatDetails prints every finding for the audit log file.
func (f *TableFormatter) FormatDetails(w io.Writer, findings []Finding, summary Summary) error {
	// Define colors
	critical := color.New(color.FgRed, color.Bold)
	high := color.New(color.FgRed)
	medium := color.New(color.FgYellow)
	low := color.New(color.FgCyan)
	header := color.New(color.FgWhite, color.Bold)
	dim := color.New(color.Faint)

	// Print header
	header.Fprintln(w)
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	header.Fprintln(w, "                    BILLING CONSISTENCY AUDIT")
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Check IDs are internal OpenRails audit labels, not payment-network or accounting standards.")
	fmt.Fprintln(w)

	if len(findings) == 0 {
		color.New(color.FgGreen, color.Bold).Fprintln(w, "✓ No consistency issues found!")
		return nil
	}

	// Group findings by category
	byCategory := make(map[string][]Finding)
	for _, f := range findings {
		cat := categoryFromCheckID(f.CheckID)
		byCategory[cat] = append(byCategory[cat], f)
	}

	// Print findings by category
	categoryOrder := []string{
		"subscription_entitlement",
		"payment_entitlement",
		"duplicates",
		"subscription_state",
		"entitlement_state",
		"payment_method",
		"foreign_key",
		"admin_grant",
		"temporal",
	}

	for _, cat := range categoryOrder {
		catFindings, ok := byCategory[cat]
		if !ok || len(catFindings) == 0 {
			continue
		}

		header.Fprintf(w, "\n━━━ %s (%d issues) ━━━\n", strings.ToUpper(strings.ReplaceAll(cat, "_", " ")), len(catFindings))

		for _, finding := range catFindings {
			// Print severity with color
			var severityColor *color.Color
			switch finding.Severity {
			case SeverityCritical:
				severityColor = critical
			case SeverityHigh:
				severityColor = high
			case SeverityMedium:
				severityColor = medium
			default:
				severityColor = low
			}

			fmt.Fprintln(w, "")
			severityColor.Fprintf(w, "[%s] %s\n", finding.Severity, finding.CheckID)
			fmt.Fprintf(w, "  Entity: %s %s\n", finding.EntityType, finding.EntityID)
			if finding.UserID != "" {
				fmt.Fprintf(w, "  User:   %s\n", finding.UserID)
			}
			fmt.Fprintf(w, "  Issue:  %s\n", finding.Description)
			dim.Fprintf(w, "  Fix:    %s", finding.Recommendation)
			if finding.AutoFixable {
				color.New(color.FgGreen).Fprint(w, " [auto-fixable]")
			}
			fmt.Fprintln(w)
		}
	}

	// Print summary
	header.Fprintln(w)
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	header.Fprintln(w, "                           SUMMARY")
	header.Fprintln(w, "═══════════════════════════════════════════════════════════════════")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "Total findings:     %d\n", summary.TotalFindings)
	fmt.Fprintf(w, "Auto-fixable:       %d\n", summary.AutoFixable)
	fmt.Fprintf(w, "Manual review:      %d\n", summary.ManualReviewOnly)

	fmt.Fprintln(w, "\nBy Severity:")
	if v := summary.BySeverity[SeverityCritical]; v > 0 {
		critical.Fprintf(w, "  CRITICAL: %d\n", v)
	}
	if v := summary.BySeverity[SeverityHigh]; v > 0 {
		high.Fprintf(w, "  HIGH:     %d\n", v)
	}
	if v := summary.BySeverity[SeverityMedium]; v > 0 {
		medium.Fprintf(w, "  MEDIUM:   %d\n", v)
	}
	if v := summary.BySeverity[SeverityLow]; v > 0 {
		low.Fprintf(w, "  LOW:      %d\n", v)
	}

	fmt.Fprintln(w, "\nBy Category:")
	for cat, count := range summary.ByCategory {
		fmt.Fprintf(w, "  %-25s %d\n", cat+":", count)
	}
	fmt.Fprintln(w)

	return nil
}

type checkCount struct {
	id    string
	name  string
	count int
}

func checkCounts(findings []Finding) []checkCount {
	byID := make(map[string]*checkCount)
	for _, f := range findings {
		row := byID[f.CheckID]
		if row == nil {
			row = &checkCount{id: f.CheckID, name: f.CheckName}
			byID[f.CheckID] = row
		}
		row.count++
	}

	rows := make([]checkCount, 0, len(byID))
	for _, row := range byID {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].id < rows[j].id
	})
	return rows
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func categoryFromCheckID(checkID string) string {
	// Extract category from check ID prefix
	switch {
	case strings.HasPrefix(checkID, "S-E-"):
		return "subscription_entitlement"
	case strings.HasPrefix(checkID, "P-E-"):
		return "payment_entitlement"
	case strings.HasPrefix(checkID, "D-"):
		return "duplicates"
	case strings.HasPrefix(checkID, "SS-"):
		return "subscription_state"
	case strings.HasPrefix(checkID, "ES-"):
		return "entitlement_state"
	case strings.HasPrefix(checkID, "PM-"):
		return "payment_method"
	case strings.HasPrefix(checkID, "FK-"):
		return "foreign_key"
	case strings.HasPrefix(checkID, "AG-"):
		return "admin_grant"
	case strings.HasPrefix(checkID, "T-"):
		return "temporal"
	default:
		return "other"
	}
}

// JSONFormatter outputs results as JSON
type JSONFormatter struct{}

type jsonOutput struct {
	Findings []Finding `json:"findings"`
	Summary  Summary   `json:"summary"`
}

func (f *JSONFormatter) Format(w io.Writer, findings []Finding, summary Summary) error {
	output := jsonOutput{
		Findings: findings,
		Summary:  summary,
	}

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

// CSVFormatter outputs results as CSV
type CSVFormatter struct{}

func (f *CSVFormatter) Format(w io.Writer, findings []Finding, summary Summary) error {
	csvWriter := csv.NewWriter(w)
	defer csvWriter.Flush()

	// Write header
	header := []string{
		"check_id",
		"check_name",
		"severity",
		"entity_type",
		"entity_id",
		"user_id",
		"description",
		"recommendation",
		"auto_fixable",
	}
	if err := csvWriter.Write(header); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	// Write findings
	for _, finding := range findings {
		row := []string{
			finding.CheckID,
			finding.CheckName,
			string(finding.Severity),
			string(finding.EntityType),
			finding.EntityID.String(),
			finding.UserID,
			finding.Description,
			finding.Recommendation,
			fmt.Sprintf("%t", finding.AutoFixable),
		}
		if err := csvWriter.Write(row); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}

	return nil
}
