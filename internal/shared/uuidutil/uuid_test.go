package uuidutil

import "testing"

func TestNewV7(t *testing.T) {
	id := NewV7()
	if got := id.Version(); got != 7 {
		t.Fatalf("NewV7() version = %d, want 7", got)
	}
}
