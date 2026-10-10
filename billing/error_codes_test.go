package billing

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var snakeCode = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// The registry is the code contract: one status and type per code, and every
// Code constant this package exports is in it.
func TestErrorCodeRegistry(t *testing.T) {
	types := map[string]bool{ErrorTypeInvalidRequest: true, ErrorTypeAuthentication: true, ErrorTypeAuthorization: true, ErrorTypeCard: true, ErrorTypeRateLimit: true, ErrorTypeAPI: true}
	previous := ""
	for _, c := range ErrorCodes() {
		require.Regexp(t, snakeCode, c.Code)
		require.Greater(t, c.Code, previous, "ErrorCodes is sorted and unique")
		previous = c.Code
		require.True(t, types[c.Type], "%s: unknown type %s", c.Code, c.Type)
		require.NotEmpty(t, http.StatusText(c.Status), c.Code)
		if c.Status >= 500 {
			require.Equal(t, ErrorTypeAPI, c.Type, "%s: a 5xx is an api_error", c.Code)
		}
		got, ok := LookupErrorCode(c.Code)
		require.True(t, ok)
		require.Equal(t, c, got)
	}
	_, ok := LookupErrorCode("no_such_code")
	require.False(t, ok)

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	declared := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Code") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					_, registered := LookupErrorCode(value)
					require.True(t, registered, "%s = %q is not in the registry", id.Name, value)
					declared++
				}
			}
		}
	}
	require.NotZero(t, declared)
}
