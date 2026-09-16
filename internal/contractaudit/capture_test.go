package contractaudit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureDetectsContractMutations(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"pkg", "internal/http/routes", "migrations/postgres"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	put := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	const original = "package fixture\nimport uuid \"example.test/uuid/v1\"\ntype Request struct { ID uuid.ID `json:\"id\"`; Amount int64 `json:\"amount,string\"` }\nfunc Call(r Request) error {return nil}\nconst( First=iota; Second )\n"
	put("api.go", original)
	put("internal/http/routes/routes.go", "package routes\n// permission: invoices:read\n")
	put("migrations/postgres/0001_schema.up.sql", "CREATE TABLE invoice (id uuid PRIMARY KEY);\n")
	before, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, path, contents string }{
		{"Go return type", "api.go", "package fixture\nfunc Call() string {return \"\"}\n"},
		{"wire field", "api.go", "package fixture\ntype Request struct { Amount int64 `json:\"amount\"` }\n"},
		{"permission", "internal/http/routes/routes.go", "package routes\n// permission: invoices:update\n"},
		{"schema", "migrations/postgres/0001_schema.up.sql", "CREATE TABLE invoice (id text PRIMARY KEY);\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.path)
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.WriteFile(path, saved, 0644); err != nil {
					t.Fatal(err)
				}
			}()
			put(tc.path, tc.contents)
			after, err := Capture(root)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(before, after) {
				t.Fatal("contract mutation was not detected")
			}
		})
	}
	put("api.go", original+"func internalOnly() {}\n")
	after, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("unexported implementation change altered public API")
	}
}
