package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestPushCommandsRejectOtherManifestShapes(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
		body string
		want string
	}{
		{
			name: "authkit authority rejects merchants",
			cmd:  newPushAuthBootstrapCmd(),
			body: "merchants: []\n",
			want: "merchants",
		},
		{
			name: "merchant config rejects authkit authority",
			cmd:  newPushMerchantConfigCmd(),
			body: "users:\n  - username: operator\n",
			want: "users",
		},
		{
			name: "catalog rejects merchants",
			cmd:  newApplyCatalogCmd(),
			body: "schema_version: 1\napplication_id: invalid-authority\nexpected_revision: 0\nmerchants: []\n",
			want: "merchants",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			tt.cmd.SetOut(&out)
			tt.cmd.SetErr(&out)
			args := []string{"--file", path}
			if tt.cmd.Name() == "apply-catalog" {
				args = append(args, "--merchant", "example")
			}
			tt.cmd.SetArgs(args)
			err := tt.cmd.Execute()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestPushCommandsHaveNoDryRunFlag(t *testing.T) {
	apply := newApplyCatalogCmd()
	for _, retired := range []string{"dry-run", "insert", "overwrite", "prune", "force"} {
		if apply.Flags().Lookup(retired) != nil {
			t.Fatalf("apply-catalog must not declare --%s; intent belongs in the document", retired)
		}
	}
	if apply.Flags().Lookup("merchant") == nil {
		t.Fatal("apply-catalog requires an explicit merchant selector")
	}
	cfg := newPushMerchantConfigCmd()
	if cfg.Flags().Lookup("dry-run") != nil {
		t.Fatal("push-merchant-config must not declare --dry-run")
	}
	if newPushAuthBootstrapCmd().Flags().Lookup("dry-run") == nil {
		t.Fatal("push-auth-bootstrap must declare its plan-only --dry-run flag")
	}
}
