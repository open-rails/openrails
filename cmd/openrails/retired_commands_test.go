package main

import (
	"bytes"
	"strings"
	"testing"
)

// or#893 phase 8: ONE name per command. A cobra Alias made two spellings work
// forever; a retired name now prints the rename and exits nonzero.
func TestRetiredCommandNamesRefuseWithTheRename(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"server"}, `"server" was renamed to "run-server" (or#893): run ` + "`openrails run-server`"},
		{[]string{"push-catalog", "--insert"}, `"push-catalog" was renamed to "push-merchant-catalog" (or#893): run ` + "`openrails push-merchant-catalog`"},
		{[]string{"dump-catalog", "--slug", "acme"}, `"dump-catalog" was renamed to "dump-merchant-catalog" (or#893): run ` + "`openrails dump-merchant-catalog`"},
	}

	for _, tc := range cases {
		t.Run(tc.args[0], func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil {
				t.Fatal("a retired command name must exit nonzero")
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// The surviving names are the ONLY ones in --help: a retired stub is hidden, so
// nothing points an operator back at the name that no longer works.
func TestRetiredCommandNamesAreNotAdvertised(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help: %v", err)
	}
	help := out.String()
	for retired := range retiredCommands {
		for _, line := range strings.Split(help, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), retired+" ") {
				t.Fatalf("--help advertises the retired name %q: %q", retired, line)
			}
		}
	}
	for _, surviving := range []string{"run-server", "push-merchant-catalog", "dump-merchant-catalog"} {
		if !strings.Contains(help, surviving) {
			t.Fatalf("--help must list %q", surviving)
		}
	}
}

// or#893 phase 8: plan-only means "declared no mutation class". The hidden
// --dry-run flag that also meant that is gone — it could override an explicitly
// requested mutation, so the same invocation meant two different things.
func TestPushCommandsHaveNoDryRunFlag(t *testing.T) {
	push := newPushCatalogCmd()
	if push.Flags().Lookup("dry-run") != nil {
		t.Fatal("push-merchant-catalog must not declare --dry-run")
	}
	cfg := newPushMerchantConfigCmd()
	if cfg.Flags().Lookup("dry-run") != nil {
		t.Fatal("push-merchant-config must not declare --dry-run")
	}
	// push-auth-bootstrap keeps its --dry-run: it declares no mutation classes,
	// so the flag is the ONLY way to plan, not a second spelling of one.
	if newPushAuthBootstrapCmd().Flags().Lookup("dry-run") == nil {
		t.Fatal("push-auth-bootstrap --dry-run is its plan mode and must stay")
	}
}
