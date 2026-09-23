package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantConfigurationRemoteCLI(t *testing.T) {
	// Any local config load would reject this retired input. Remote mode has
	// no local infrastructure dependency, even with hostile local settings.
	t.Setenv("ENV", "retired")
	tokenPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("test-credential"), 0600))
	document := filepath.Join(t.TempDir(), "application.json")
	require.NoError(t, os.WriteFile(document, []byte(`{"application_id":"stable-op","expected_revision":"before","display_name":"New name"}`), 0600))
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer test-credential", r.Header.Get("Authorization"))
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"application_id":"stable-op","revision":"after","replayed":false}`))
		} else {
			_, _ = w.Write([]byte(`{"revision":"before","display_name":"Old name","settings":{}}`))
		}
	}))
	defer server.Close()
	for _, command := range []string{"get-merchant-config", "apply-merchant-config"} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		args := []string{command, "--merchant", "shop", "--server-url", server.URL, "--token-file", tokenPath}
		if command == "apply-merchant-config" {
			args = append(args, "--file", document)
		}
		root.SetArgs(args)
		require.NoError(t, root.Execute())
		require.Contains(t, out.String(), "revision")
	}
	require.Len(t, paths, 2)
	require.Contains(t, paths[0], "/configuration")
	require.Contains(t, paths[1], "/configuration/applications")
	root := newRootCmd()
	root.SetArgs([]string{"get-merchant-config", "--merchant", "shop", "--server-url", server.URL, "--token-file", tokenPath, "--config", "local.yaml"})
	require.ErrorContains(t, root.Execute(), "local-only")
	require.Nil(t, newDumpMerchantConfigCmd().Flags().Lookup("include-secrets"))
}
