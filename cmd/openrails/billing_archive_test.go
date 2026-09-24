package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func requireNoArchiveTemporaries(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".openrails-billing-*"))
	require.NoError(t, err)
	require.Empty(t, matches)
}

// A snapshot is private and published only when complete; an existing or
// concurrently created destination is never clobbered without --overwrite.
func TestBillingArchivePublication(t *testing.T) {
	for name, tc := range map[string]struct {
		existing  string
		overwrite bool
		race      bool
		exportErr error
		want      string
		wantErr   string
	}{
		"new":                    {want: "snapshot"},
		"overwrite":              {existing: "previous", overwrite: true, want: "snapshot"},
		"failed new":             {exportErr: io.ErrUnexpectedEOF},
		"failed overwrite":       {existing: "previous", overwrite: true, exportErr: io.ErrUnexpectedEOF, want: "previous"},
		"existing without flag":  {existing: "other writer", want: "other writer", wantErr: "--overwrite"},
		"appears during export":  {race: true, want: "other writer", wantErr: "--overwrite"},
		"stdout is not a target": {wantErr: "standard streams"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "snapshot.jsonl")
			if name == "stdout is not a target" {
				path = "-"
			}
			if tc.existing != "" {
				require.NoError(t, os.WriteFile(path, []byte(tc.existing), 0o644))
			}
			exported := false
			err := writeBillingArchive(path, tc.overwrite, func(w io.Writer) error {
				exported = true
				info, err := w.(*os.File).Stat()
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
				if tc.existing == "" {
					_, statErr := os.Stat(path)
					require.ErrorIs(t, statErr, os.ErrNotExist, "destination visible before completion")
				} else {
					requireFile(t, path, tc.existing)
				}
				if tc.race {
					require.NoError(t, os.WriteFile(path, []byte("other writer"), 0o600))
				}
				_, _ = io.WriteString(w, "snapshot")
				return tc.exportErr
			})
			switch {
			case tc.exportErr != nil:
				require.ErrorIs(t, err, tc.exportErr)
			case tc.wantErr != "":
				require.ErrorContains(t, err, tc.wantErr)
			default:
				require.NoError(t, err)
			}
			if tc.existing != "" && !tc.overwrite {
				require.False(t, exported, "an existing destination is refused before exporting")
			}
			if tc.want == "" {
				if path != "-" {
					_, statErr := os.Stat(path)
					require.ErrorIs(t, statErr, os.ErrNotExist)
				}
			} else {
				requireFile(t, path, tc.want)
			}
			if tc.want == "snapshot" {
				info, err := os.Stat(path)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
			requireNoArchiveTemporaries(t, dir)
		})
	}
}

func TestBillingArchiveReplacesSymlinkNotTarget(t *testing.T) {
	dir := t.TempDir()
	target, path := filepath.Join(dir, "unrelated"), filepath.Join(dir, "snapshot.jsonl")
	require.NoError(t, os.WriteFile(target, []byte("untouched"), 0o600))
	require.NoError(t, os.Symlink(target, path))
	write := func(w io.Writer) error { _, err := io.WriteString(w, "snapshot"); return err }
	require.Error(t, writeBillingArchive(path, false, write), "a dangling or live symlink is an existing destination")
	require.NoError(t, writeBillingArchive(path, true, write))
	requireFile(t, target, "untouched")
	requireFile(t, path, "snapshot")
}

func TestBillingArchiveFlagsRejectedBeforeOpeningRuntime(t *testing.T) {
	const mid = "ab181b77-1f3b-4bde-a7a7-a2a42f3c07ae"
	for name, tc := range map[string]struct {
		cmd  func() *cobra.Command
		args []string
		want string
	}{
		"missing merchant":           {newBillingExportCmd, []string{"--out", "s.jsonl"}, "--merchant"},
		"slug merchant":              {newBillingExportCmd, []string{"--merchant", "shop", "--out", "s.jsonl"}, "exact UUID"},
		"zero merchant":              {newBillingImportCmd, []string{"--merchant", "id:00000000-0000-0000-0000-000000000000", "--in", "s.jsonl"}, "nonzero"},
		"url without token":          {newBillingExportCmd, []string{"--merchant", mid, "--url", "https://example.invalid", "--out", "s.jsonl"}, "together"},
		"token without url":          {newBillingImportCmd, []string{"--merchant", mid, "--token-file", "secret", "--in", "s.jsonl"}, "together"},
		"negative timeout":           {newBillingImportCmd, []string{"--merchant", mid, "--timeout=-1s", "--in", "s.jsonl"}, "negative"},
		"stdout":                     {newBillingExportCmd, []string{"--merchant", mid, "--out=-"}, "standard streams"},
		"stdin":                      {newBillingImportCmd, []string{"--merchant", mid, "--in=-"}, "standard streams"},
		"missing output":             {newBillingExportCmd, []string{"--merchant", mid}, "--out"},
		"source writers not stopped": {newBillingExportCmd, []string{"--merchant", mid, "--out", "s.jsonl"}, "--source-stopped"},
		"missing input":              {newBillingImportCmd, []string{"--merchant", mid}, "--in"},
		"input is a directory":       {newBillingImportCmd, []string{"--merchant", mid, "--in", "."}, "regular file"},
		"extra argument":             {newBillingImportCmd, []string{"extra"}, "unknown command"},
		"prepare without owner":      {newBillingPrepareTargetCmd, []string{"--merchant", mid, "--authkit-group-id", mid}, "--owner-user-id"},
		"prepare mixed identity":     {newBillingPrepareTargetCmd, []string{"--merchant", mid, "--unbound-merchants", "--slug", "shop", "--owner-user-id", mid}, "cannot use"},
		"prepare unbound no slug":    {newBillingPrepareTargetCmd, []string{"--merchant", mid, "--unbound-merchants"}, "requires --slug"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := tc.cmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
	require.NoError(t, validateBillingPrepareTarget(true, "shop", "", ""))
	require.NoError(t, validateBillingPrepareTarget(false, "", "group", "owner"))
}

func TestBillingArchiveBearerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	for _, token := range []string{"", " \n", "first\nsecond", "two words", strings.Repeat("x", (64<<10)+1)} {
		require.NoError(t, os.WriteFile(path, []byte(token), 0o600))
		_, err := readBillingArchiveToken(path)
		require.Error(t, err, "%q", token)
	}
	require.NoError(t, os.WriteFile(path, []byte("test-bearer\n"), 0o600))
	token, err := readBillingArchiveToken(path)
	require.NoError(t, err)
	require.Equal(t, "test-bearer", token)
	_, err = readBillingArchiveToken(path + ".missing")
	require.Error(t, err)
}

// Remote archive operations need only a URL and bearer file; local
// configuration must not become an accidental dependency, nor be skipped
// for local operations.
func TestBillingArchiveRemoteSkipsLocalConfiguration(t *testing.T) {
	for _, remote := range []bool{false, true} {
		configurationError := errors.New("local configuration requested")
		root := &cobra.Command{Use: "openrails", PersistentPreRunE: func(*cobra.Command, []string) error { return configurationError }}
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
			require.ErrorContains(t, err, "--out")
		} else {
			require.ErrorIs(t, err, configurationError)
		}
	}
}
