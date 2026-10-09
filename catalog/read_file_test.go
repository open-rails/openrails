package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	yamlApp, err := ReadFile(filepath.Join("..", "examples", "embedded", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(yamlApp.Products) != 4 {
		t.Fatalf("the example catalog has 4 products, read %d", len(yamlApp.Products))
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
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
