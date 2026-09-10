package server_test

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandaloneRouteReferenceMatchesDocumentation(t *testing.T) {
	root := repositoryRoot(t)
	want := readRouteLines(t, filepath.Join(root, "internal/integrationharness/testdata/standalone_route_surface.txt"))
	got := documentedStandaloneRoutes(t, filepath.Join(root, "docs/api/endpoints.md"))
	require.Equal(t, want, got, "docs/api/endpoints.md must list every standalone route and no stale routes")
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func readRouteLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })

	var routes []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			routes = append(routes, line)
		}
	}
	require.NoError(t, scanner.Err())
	sort.Strings(routes)
	return routes
}

func documentedStandaloneRoutes(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, f.Close()) })

	routeSet := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		cells := strings.Split(scanner.Text(), "|")
		if len(cells) < 4 {
			continue
		}
		methods := strings.Split(strings.TrimSpace(cells[1]), "/")
		if !routeMethods(methods) {
			continue
		}
		for _, path := range codeSpans(cells[2]) {
			if strings.HasPrefix(path, "/billing/") {
				continue
			}
			if path == "/" {
				path = "/{$}"
			}
			for _, method := range methods {
				routeSet[method+" "+path] = struct{}{}
			}
		}
	}
	require.NoError(t, scanner.Err())

	routes := make([]string, 0, len(routeSet))
	for route := range routeSet {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	return routes
}

func routeMethods(methods []string) bool {
	if len(methods) == 0 {
		return false
	}
	for _, method := range methods {
		switch method {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			return false
		}
	}
	return true
}

func codeSpans(cell string) []string {
	var spans []string
	for {
		start := strings.IndexByte(cell, '`')
		if start < 0 {
			return spans
		}
		cell = cell[start+1:]
		end := strings.IndexByte(cell, '`')
		if end < 0 {
			return spans
		}
		spans = append(spans, cell[:end])
		cell = cell[end+1:]
	}
}
