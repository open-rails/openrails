package main

import (
	"os"
	"regexp"
	"testing"
)

// The account run-server guards is the one `task dev` seeds.
func TestTheGuardedOperatorIsTheOneTaskDevSeeds(t *testing.T) {
	raw, err := os.ReadFile("../../../Taskfile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s+DEV_OPERATOR_EMAIL:\s*(\S+)\s*$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("Taskfile.yaml names no DEV_OPERATOR_EMAIL")
	}
	if string(m[1]) != devOperatorEmail {
		t.Fatalf("Taskfile.yaml's DEV_OPERATOR_EMAIL is %s, want %s", m[1], devOperatorEmail)
	}
}
