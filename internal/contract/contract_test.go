package contract

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/routes"
)

// The committed OpenAPI document, TypeScript types and API tables are what
// the route catalog renders today. A route, a wire type or an error code
// cannot change without its generated files changing in the same commit.
func TestGeneratedContractIsFresh(t *testing.T) {
	require.NoError(t, Verify(os.DirFS("../..")))
}

// Every route and every registered code is in the OpenAPI document, and every
// schema it references is defined.
func TestOpenAPICoversTheCatalog(t *testing.T) {
	files, err := Files(os.DirFS("../.."))
	require.NoError(t, err)
	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
		Codes map[string]json.RawMessage `json:"x-openrails-error-codes"`
		Sets  map[string][]string        `json:"x-openrails-error-sets"`
	}
	require.NoError(t, json.Unmarshal(files[OpenAPIFile], &doc))
	for _, r := range routes.Catalog() {
		require.Contains(t, doc.Paths[r.Path], strings.ToLower(r.Method), r.Key())
		for _, set := range r.ErrorSets() {
			require.Contains(t, doc.Sets, set, r.Key())
		}
	}
	require.Len(t, doc.Codes, len(billing.ErrorCodes()))
	for _, set := range doc.Sets {
		for _, code := range set {
			require.Contains(t, doc.Codes, code)
		}
	}

	text := string(files[OpenAPIFile])
	for _, ref := range strings.Split(text, `"$ref": "#/components/schemas/`)[1:] {
		name := ref[:strings.Index(ref, `"`)]
		require.Contains(t, doc.Components.Schemas, name, "referenced schema is not defined")
	}
	require.Contains(t, doc.Components.Schemas, "Error")
}

// Each generated TypeScript file declares every type it names.
func TestTypeScriptIsClosed(t *testing.T) {
	files, err := Files(os.DirFS("../.."))
	require.NoError(t, err)
	for _, name := range []string{billingUIDir + "wire.ts", consoleDir + "wire.ts"} {
		src := string(files[name])
		declared := map[string]bool{"ListPage": true, "Record": true, "T": true}
		for _, line := range strings.Split(src, "\n") {
			if rest, ok := strings.CutPrefix(line, "export type "); ok {
				declared[strings.FieldsFunc(rest, func(r rune) bool { return r == ' ' || r == '<' })[0]] = true
			}
		}
		for _, line := range strings.Split(src, "\n") {
			if !strings.HasPrefix(line, "  ") || !strings.Contains(line, ": ") {
				continue
			}
			typ := line[strings.Index(line, ": ")+2:]
			for _, word := range strings.FieldsFunc(typ, func(r rune) bool {
				return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '"')
			}) {
				if word[0] >= 'A' && word[0] <= 'Z' {
					require.True(t, declared[word], "%s: %s is not declared (%s)", name, word, strings.TrimSpace(line))
				}
			}
		}
	}
}
