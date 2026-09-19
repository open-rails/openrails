package app

import (
	"context"
	"testing"
)

type closingControlPlane struct{ calls int }

func (c *closingControlPlane) Close() { c.calls++ }

func TestCloseReleasesAttachedControlPlane(t *testing.T) {
	cp := &closingControlPlane{}
	a := &App{}
	a.SetControlPlane(cp, nil)
	for i := 0; i < 2; i++ {
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if cp.calls != 1 {
		t.Fatalf("control plane closed %d times, want 1", cp.calls)
	}
	if a.ControlPlane != nil {
		t.Fatal("closed control plane remains attached")
	}
}
