//go:build gatefixture_fail

package gatefixture

import "testing"

func TestGateFixtureFails(t *testing.T) {
	t.Fatal("fixture: the wrapper must surface a real failure")
}
