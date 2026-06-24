package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMutationFlagsAreExposedOnOperatorCommands(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "pull-provider", cmd: newPullProviderCmd()},
		{name: "push-bootstrap", cmd: newPushBootstrapCmd()},
		{name: "push-merchant-config", cmd: newPushMerchantConfigCmd()},
		{name: "push-merchant-catalog", cmd: newPushCatalogCmd()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{"insert", "overwrite", "prune"} {
				if tt.cmd.Flags().Lookup(name) == nil {
					t.Fatalf("missing --%s", name)
				}
			}
			if tt.name == "pull-provider" && tt.cmd.Flags().Lookup("log-dir") == nil {
				t.Fatalf("pull-provider missing --log-dir")
			}
		})
	}
}

func TestDryRunFlagHiddenWhereRetainedAsCompatibilityAlias(t *testing.T) {
	for _, cmd := range []*cobra.Command{
		newPushBootstrapCmd(),
		newPushMerchantConfigCmd(),
		newPushCatalogCmd(),
	} {
		flag := cmd.Flags().Lookup("dry-run")
		if flag == nil {
			t.Fatalf("%s missing hidden --dry-run compatibility flag", cmd.Use)
		}
		if !flag.Hidden {
			t.Fatalf("%s --dry-run should be hidden; visible mutation contract is --insert/--overwrite/--prune", cmd.Use)
		}
	}
}

func TestBootstrapAndMerchantConfigCommandsAreDistinct(t *testing.T) {
	bootstrapCmd := newPushBootstrapCmd()
	merchantConfigCmd := newPushMerchantConfigCmd()

	if bootstrapCmd.Use != "push-bootstrap" {
		t.Fatalf("bootstrap command use = %q", bootstrapCmd.Use)
	}
	if merchantConfigCmd.Use != "push-merchant-config" {
		t.Fatalf("merchant config command use = %q", merchantConfigCmd.Use)
	}
	if len(bootstrapCmd.Aliases) != 0 {
		t.Fatalf("push-bootstrap should not be an alias wrapper, got aliases %v", bootstrapCmd.Aliases)
	}
	if len(merchantConfigCmd.Aliases) != 0 {
		t.Fatalf("push-merchant-config should not alias push-bootstrap, got aliases %v", merchantConfigCmd.Aliases)
	}
}

func TestPushCommandsRejectOtherManifestShapes(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
		body string
		want string
	}{
		{
			name: "bootstrap rejects merchants",
			cmd:  newPushBootstrapCmd(),
			body: "version: 1\nmerchants: []\n",
			want: "merchants",
		},
		{
			name: "merchant config rejects bootstrap authority",
			cmd:  newPushMerchantConfigCmd(),
			body: "version: 1\nauthority:\n  bootstrap_org_slug: local-stack\n",
			want: "authority",
		},
		{
			name: "catalog rejects merchants",
			cmd:  newPushCatalogCmd(),
			body: "version: 1\nmerchants: []\n",
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
			tt.cmd.SetArgs([]string{"--file", path})
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
