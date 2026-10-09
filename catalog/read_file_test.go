package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	yamlApp, err := ReadFile(write("catalog.yaml", "schema_version: 1\nproducts:\n  pass:\n    display_name: Pass\n  plus:\n    display_name: Plus\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(yamlApp.Products) != 2 {
		t.Fatalf("read %d products, want 2", len(yamlApp.Products))
	}
	jsonApp, err := ReadFile(write("catalog.JSON", `{"schema_version":1,"products":{"pass":{"display_name":"Pass"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if jsonApp.Products["pass"].DisplayName != Value("Pass") {
		t.Fatalf("read %+v", jsonApp.Products)
	}
	if _, err := ReadFile(write("catalog.yml", "schema_version: 1\nproducts:\n  pass:\n    display_name: Pass\n")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"catalog.toml":   "schema_version = 1",
		"unknown.yaml":   "schema_version: 1\nproduct: []\n",
		"invalid.json":   "{",
		"no-version.yml": "products: {}\n",
		"list form.yml":  "schema_version: 1\nproducts:\n  - key: pass\n    display_name: Pass\n",
	} {
		if _, err := ReadFile(write(name, body)); err == nil {
			t.Errorf("%s: read without error", name)
		}
	}
	if _, err := ReadFile(filepath.Join(dir, "missing.yaml")); !os.IsNotExist(err) {
		t.Errorf("missing file: %v", err)
	}
}
