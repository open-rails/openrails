package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestBillingArchivePublication(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "overwrite"}[overwrite], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot.jsonl")
			if overwrite {
				if err := os.WriteFile(path, []byte("previous"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := writeBillingArchive(path, overwrite, func(w io.Writer) error {
				f := w.(*os.File)
				info, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("temporary permissions = %o", info.Mode().Perm())
				}
				if overwrite {
					assertArchiveContents(t, path, "previous")
				} else if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("destination visible before completion: %v", err)
				}
				_, err = io.WriteString(w, "complete snapshot")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			assertArchiveContents(t, path, "complete snapshot")
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("destination permissions: info=%v err=%v", info, err)
			}
			assertNoArchiveTemporaryFiles(t, dir)
		})
	}
}

func TestBillingArchiveFailedExportPreservesDestination(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot.jsonl")
			if existing {
				if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			failure := io.ErrUnexpectedEOF
			err := writeBillingArchive(path, existing, func(w io.Writer) error {
				_, _ = io.WriteString(w, "partial sensitive data")
				return failure
			})
			if !errors.Is(err, failure) {
				t.Fatalf("export error = %v", err)
			}
			if existing {
				assertArchiveContents(t, path, "previous")
			} else if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed export published destination: %v", err)
			}
			assertNoArchiveTemporaryFiles(t, dir)
		})
	}
}

func TestBillingArchiveRejectsClobber(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "concurrent"}[concurrent], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot.jsonl")
			createExisting := func() {
				if err := os.WriteFile(path, []byte("other writer"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !concurrent {
				createExisting()
			}
			err := writeBillingArchive(path, false, func(w io.Writer) error {
				if !concurrent {
					t.Fatal("existing destination should fail before export")
				}
				createExisting()
				_, err := io.WriteString(w, "new snapshot")
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "--overwrite") {
				t.Fatalf("expected overwrite refusal, got %v", err)
			}
			assertArchiveContents(t, path, "other writer")
			assertNoArchiveTemporaryFiles(t, dir)
		})
	}
}

func TestBillingArchiveSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unrelated")
	path := filepath.Join(dir, "snapshot.jsonl")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	write := func(w io.Writer) error { _, err := io.WriteString(w, "snapshot"); return err }
	if err := writeBillingArchive(path, false, write); err == nil {
		t.Fatal("existing symlink accepted without --overwrite")
	}
	if err := writeBillingArchive(path, true, write); err != nil {
		t.Fatal(err)
	}
	assertArchiveContents(t, target, "untouched")
	assertArchiveContents(t, path, "snapshot")
}

func TestBillingArchiveFlagsRejectInvalidInputBeforeOpeningRuntime(t *testing.T) {
	mid := "ab181b77-1f3b-4bde-a7a7-a2a42f3c07ae"
	tests := []struct {
		name string
		cmd  *cobra.Command
		args []string
		want string
	}{
		{"missing merchant", newBillingExportCmd(), []string{"--out", "snapshot.jsonl"}, "--merchant"},
		{"slug", newBillingExportCmd(), []string{"--merchant", "shop", "--out", "snapshot.jsonl"}, "exact UUID"},
		{"zero merchant", newBillingImportCmd(), []string{"--merchant", "00000000-0000-0000-0000-000000000000", "--in", "snapshot.jsonl"}, "nonzero"},
		{"url without token", newBillingExportCmd(), []string{"--merchant", mid, "--url", "https://example.invalid", "--out", "snapshot.jsonl"}, "together"},
		{"token without url", newBillingImportCmd(), []string{"--merchant", mid, "--token-file", "secret", "--in", "snapshot.jsonl"}, "together"},
		{"negative timeout", newBillingImportCmd(), []string{"--merchant", mid, "--timeout=-1s", "--in", "snapshot.jsonl"}, "negative"},
		{"stdout", newBillingExportCmd(), []string{"--merchant", mid, "--out=-"}, "standard streams"},
		{"stdin", newBillingImportCmd(), []string{"--merchant", mid, "--in=-"}, "standard streams"},
		{"missing output", newBillingExportCmd(), []string{"--merchant", mid}, "--out"},
		{"missing source attestation", newBillingExportCmd(), []string{"--merchant", mid, "--out", "snapshot.jsonl"}, "--source-stopped"},
		{"missing input", newBillingImportCmd(), []string{"--merchant", mid}, "--in"},
		{"extra argument", newBillingImportCmd(), []string{"extra"}, "unknown command"},
		{"prepare missing owner", newBillingPrepareTargetCmd(), []string{"--merchant", mid, "--authkit-group-id", mid}, "--owner-user-id"},
		{"prepare mixed identity", newBillingPrepareTargetCmd(), []string{"--merchant", mid, "--unbound-merchants", "--slug", "shop", "--owner-user-id", mid}, "cannot use"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cmd.SetOut(io.Discard)
			tt.cmd.SetErr(io.Discard)
			tt.cmd.SetArgs(tt.args)
			err := tt.cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %q", err, tt.want)
			}
		})
	}
}

func TestBillingArchiveInputAndCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := openBillingArchiveInput(dir); err == nil {
		t.Fatal("accepted directory")
	}
	if _, err := openBillingArchiveInput(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("accepted missing input")
	}
	path := filepath.Join(dir, "credential")
	for _, token := range []string{"", "first\nsecond", strings.Repeat("x", (64<<10)+1)} {
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readBillingArchiveToken(path); err == nil {
			t.Fatal("accepted invalid credential file")
		}
	}
	if err := os.WriteFile(path, []byte("test-bearer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if token, err := readBillingArchiveToken(path); err != nil || token != "test-bearer" {
		t.Fatalf("read token = %q, %v", token, err)
	}
	for _, value := range []string{"ab181b77-1f3b-4bde-a7a7-a2a42f3c07ae", "id:ab181b77-1f3b-4bde-a7a7-a2a42f3c07ae"} {
		if _, err := parseBillingArchiveMerchant(value); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateBillingPrepareTarget(true, "shop", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := validateBillingPrepareTarget(false, "", "group", "owner"); err != nil {
		t.Fatal(err)
	}
}

func TestBillingArchiveRemoteSkipsLocalConfiguration(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "remote"}[remote], func(t *testing.T) {
			configurationError := errors.New("local configuration requested")
			root := &cobra.Command{
				Use:               "openrails",
				PersistentPreRunE: func(*cobra.Command, []string) error { return configurationError },
			}
			root.AddCommand(newBillingCmd())
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			args := []string{"billing", "export", "--merchant", "ab181b77-1f3b-4bde-a7a7-a2a42f3c07ae"}
			if remote {
				args = append(args, "--url", "https://example.invalid", "--token-file", "credential")
			}
			root.SetArgs(args)
			err := root.Execute()
			if remote {
				if err == nil || !strings.Contains(err.Error(), "--out") {
					t.Fatalf("remote command used local configuration: %v", err)
				}
			} else if !errors.Is(err, configurationError) {
				t.Fatalf("local command skipped configuration: %v", err)
			}
		})
	}
}

func assertArchiveContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("file content = %q, %v; want %q", got, err, want)
	}
}

func assertNoArchiveTemporaryFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".openrails-billing-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary snapshot files = %v, %v", matches, err)
	}
}
