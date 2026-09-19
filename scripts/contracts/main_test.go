package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintFailuresIncludesTestsOutsideTheManifest(t *testing.T) {
	raw := []byte(`{"Action":"output","Package":"unlisted","Test":"TestLedger","Output":"ledger: sequence column missing\n"}
{"Action":"fail","Package":"unlisted","Test":"TestLedger"}
{"Action":"output","Package":"unlisted","Output":"FAIL unlisted\n"}
{"Action":"fail","Package":"unlisted"}
{"Action":"output","Package":"unlisted","Test":"TestPassingSibling","Output":"passing sibling noise\n"}
{"Action":"pass","Package":"unlisted","Test":"TestPassingSibling"}
{"Action":"build-output","ImportPath":"broken [test]","Output":"undefined: MissingSymbol\n"}
{"Action":"build-fail","ImportPath":"broken [test]"}
not a JSON event
`)
	var output bytes.Buffer
	printTestFailures(&output, raw)
	for _, want := range []string{"FAIL unlisted TestLedger", "ledger: sequence column missing", "FAIL unlisted", "FAIL broken [test]", "undefined: MissingSymbol"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in diagnostic output: %s", want, &output)
		}
	}
	if strings.Contains(output.String(), "passing sibling noise") {
		t.Fatal("passing test output obscures the failures")
	}
}
