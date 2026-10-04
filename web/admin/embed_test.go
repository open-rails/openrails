package admin_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/adminconsole"
	admin "github.com/open-rails/openrails/web/admin"
)

// One console build serves any mount path (#1127): index.html references its
// assets relative to the <base href> the handler rewrites, and each resolves
// under the mount. Skips without a build; scripts/check.sh gates it after
// building the console.
func TestBuiltConsoleServesAtAnyPath(t *testing.T) {
	assets := admin.FS()
	if assets == nil {
		t.Skip("web/admin/dist holds no console build (scripts/build-admin-console.sh)")
	}
	const mount = "/billing/admin"
	console, err := adminconsole.Handler(mount, adminconsole.Config{}, assets)
	require.NoError(t, err)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		console.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	index := get(mount + "/customers/42")
	require.Equal(t, http.StatusOK, index.Code)
	require.Contains(t, index.Body.String(), `<base href="/billing/admin/">`)
	base, err := url.Parse("http://console.test" + mount + "/")
	require.NoError(t, err)
	served := 0
	for _, ref := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(index.Body.String(), -1) {
		if ref[1] == base.Path {
			continue
		}
		require.Falsef(t, strings.HasPrefix(ref[1], "/"), "index.html references %q from the root, not relative to <base>", ref[1])
		target, err := base.Parse(ref[1])
		require.NoError(t, err)
		rec := get(target.Path)
		require.Equal(t, http.StatusOK, rec.Code, target.Path)
		require.NotContains(t, rec.Header().Get("Content-Type"), "text/html", "%s fell back to index.html", target.Path)
		served++
	}
	require.NotZero(t, served, "index.html references no assets")

	require.NoError(t, fs.WalkDir(assets, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(assets, path)
		require.NoError(t, err)
		require.NotContains(t, string(data), "/admin/assets/", "%s hard-codes the default mount", path)
		return nil
	}))
}
