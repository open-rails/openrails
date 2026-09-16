package contractaudit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewedReleaseContract(t *testing.T) {
	root := filepath.Join("..", "..")
	actual, err := Capture(root)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(root, "compatibility", "contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("reviewed Go/API/authority/schema contract changed; review the public diff and run go run ./scripts/contracts -write to update its snapshot")
	}
}
