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
