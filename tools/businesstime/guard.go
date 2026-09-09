// Package businesstime enforces explicit classification of physical-clock use
// in billing decision code.
package businesstime

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// GuardPaths is deliberately explicit: adding a billing plane is a reviewed
	// expansion of the invariant, not an accidental recursive scan.
	GuardPaths = []string{"internal/modules", "internal/river", "internal/http/handlers", "internal/reconcile", "internal/intents", "pkg/service"}

	physicalClockPattern = regexp.MustCompile(`time\.Now\(|\bNOW\(\)|CURRENT_TIMESTAMP|clockwork\.NewRealClock\(\)`)
	classifications      = map[string]bool{
		"business_time_boundary": true,
		"infrastructure_time":    true,
		"external_protocol_time": true,
		"db_audit_timestamp":     true,
	}
)

type allowance struct {
	file      string
	statement string
	expected  int
	seen      int
}

// Check scans root and reports every unclassified physical-clock read. The
// allowlist is exact by file, trimmed source line, and reviewed occurrence count.
func Check(root string, report io.Writer) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	allowances, err := loadAllowlist(root)
	if err != nil {
		return err
	}

	files, err := sourceFiles(root)
	if err != nil {
		return err
	}
	failures := 0
	for _, path := range files {
		if err := scanFile(root, path, allowances, report, &failures); err != nil {
			return err
		}
	}

	keys := make([]string, 0, len(allowances))
	for key := range allowances {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		a := allowances[key]
		if a.seen == a.expected {
			continue
		}
		fmt.Fprintf(report, "clock allowlist occurrence changed: %s (expected %d, found %d)\n", key, a.expected, a.seen)
		failures++
	}
	if failures > 0 {
		return errors.New("business-time guardrail failed; inject the owning clock or review the exact classified boundary")
	}
	return nil
}

func loadAllowlist(root string) (map[string]*allowance, error) {
	path := filepath.Join(root, "scripts", "business-time-allowlist.txt")
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	entries := make(map[string]*allowance)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) != 5 {
			return nil, fmt.Errorf("malformed clock allowlist entry: %s", line)
		}
		file, statement := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		classification, reason := strings.TrimSpace(parts[2]), strings.TrimSpace(parts[3])
		count, err := strconv.Atoi(strings.TrimSpace(parts[4]))
		if !classifications[classification] {
			return nil, fmt.Errorf("invalid clock classification: %s: %s", file, classification)
		}
		if statement == "" || reason == "" || err != nil || count < 1 {
			return nil, fmt.Errorf("malformed clock allowlist entry: %s", file)
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("stale clock allowlist path: %s", file)
		}
		key := file + "|" + statement
		if _, exists := entries[key]; exists {
			return nil, fmt.Errorf("duplicate clock allowlist entry: %s", file)
		}
		entries[key] = &allowance{file: file, statement: statement, expected: count}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return entries, nil
}

func sourceFiles(root string) ([]string, error) {
	var files []string
	for _, rel := range GuardPaths {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("missing scan directory: %s", rel)
		}
		err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func scanFile(root, path string, allowances map[string]*allowance, report io.Writer, failures *int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		statement := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(statement, "//") || strings.HasPrefix(statement, "*") || !physicalClockPattern.MatchString(statement) {
			continue
		}
		key := rel + "|" + statement
		if a := allowances[key]; a != nil {
			a.seen++
			continue
		}
		fmt.Fprintf(report, "%s:%d: unclassified business-time usage: %s\n", rel, line, statement)
		(*failures)++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", rel, err)
	}
	return nil
}
