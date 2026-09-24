package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func execute(cmd *cobra.Command, args ...string) (string, error) {
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// Each push command accepts only its own document shape, so authority and
// catalog state cannot be smuggled through another command's file.
func TestPushCommandsRejectOtherManifestShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd  *cobra.Command
		body string
		args []string
		want string
	}{
		"authkit authority rejects merchants": {newPushAuthBootstrapCmd(), "merchants: []\n", nil, "merchants"},
		"merchant config rejects authority":   {newPushMerchantConfigCmd(), "users:\n  - username: operator\n", nil, "users"},
		"catalog rejects merchants":           {newApplyCatalogCmd(), "schema_version: 1\napplication_id: x\nexpected_revision: 0\nmerchants: []\n", []string{"--merchant", "example"}, "merchants"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execute(tc.cmd, append([]string{"--file", writeTemp(t, "manifest.yaml", tc.body)}, tc.args...)...)
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err := execute(newPushMerchantConfigCmd(), "--file", filepath.Join(t.TempDir(), "missing.yaml"))
	require.ErrorContains(t, err, "read merchant config manifest")
}

// Mutation intent lives in documents and explicit verbs, never in flags that
// could silently widen or narrow an operation.
func TestMutationFlagSurface(t *testing.T) {
	apply := newApplyCatalogCmd()
	for _, retired := range []string{"dry-run", "insert", "overwrite", "prune", "force"} {
		require.Nil(t, apply.Flags().Lookup(retired), "apply-catalog --%s", retired)
	}
	require.NotNil(t, apply.Flags().Lookup("merchant"))
	require.Nil(t, newPushMerchantConfigCmd().Flags().Lookup("dry-run"), "a bare push is already plan-only")
	require.NotNil(t, newPushAuthBootstrapCmd().Flags().Lookup("dry-run"))
	require.Nil(t, newDumpMerchantConfigCmd().Flags().Lookup("include-secrets"))

	_, err := execute(newUndoRunCmd(), "--merchant", "shop", "--run", "x", "--apply")
	require.ErrorContains(t, err, "requires --expect-rows")
	_, err = execute(newDumpCatalogCmd(), "--slug", " ")
	require.ErrorContains(t, err, "--slug is required")
}

// Remote mode reaches the server through the public Client only: no local
// configuration is loaded, and local-only flags are refused.
func TestMerchantConfigurationRemoteCLI(t *testing.T) {
	t.Setenv("ENV", "retired") // any local config load would reject this
	tokenPath := writeTemp(t, "token", "test-credential\n")
	document := writeTemp(t, "application.json", `{"application_id":"stable-op","expected_revision":"before","display_name":"New name"}`)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-credential", r.Header.Get("Authorization"))
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"application_id":"stable-op","revision":"after","replayed":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"revision":"before","display_name":"Old name","settings":{}}`))
	}))
	defer server.Close()
	remote := []string{"--merchant", "shop", "--server-url", server.URL, "--token-file", tokenPath}

	out, err := execute(newRootCmd(), append([]string{"get-merchant-config"}, remote...)...)
	require.NoError(t, err)
	require.Contains(t, out, `"before"`)
	out, err = execute(newRootCmd(), append([]string{"apply-merchant-config", "--file", document}, remote...)...)
	require.NoError(t, err)
	require.Contains(t, out, `"after"`)
	require.Len(t, paths, 2)
	require.Contains(t, paths[0], "GET ")
	require.Contains(t, paths[0], "/configuration")
	require.Contains(t, paths[1], "POST ")
	require.Contains(t, paths[1], "/configuration/applications")

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"local config flag":      {append([]string{"get-merchant-config", "--config", "local.yaml"}, remote...), "local-only"},
		"local posture flag":     {append([]string{"get-merchant-config", "--test-mode", "live"}, remote...), "local-only"},
		"unbound selector":       {append([]string{"get-merchant-config", "--unbound-merchants"}, remote...), "local-only"},
		"remote without token":   {[]string{"get-merchant-config", "--merchant", "shop", "--server-url", server.URL}, "--token-file"},
		"token without remote":   {[]string{"get-merchant-config", "--merchant", "shop", "--token-file", tokenPath}, "requires --server-url"},
		"missing merchant":       {[]string{"get-merchant-config", "--server-url", server.URL, "--token-file", tokenPath}, "--merchant is required"},
		"apply without document": {append([]string{"apply-merchant-config"}, remote...), "--file is required"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := execute(newRootCmd(), tc.args...)
			require.ErrorContains(t, err, tc.want)
		})
	}
	require.Len(t, paths, 2, "refused invocations never reach the server")
}
