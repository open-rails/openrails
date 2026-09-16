// contracts captures or verifies the reviewed pre-v1 release contract.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"github.com/open-rails/openrails/internal/contractaudit"
	"os"
)

func main() {
	write := flag.Bool("write", false, "write the reviewed current contract")
	flag.Parse()
	actual, err := contractaudit.Capture(".")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	const path = "compatibility/contract.json"
	if *write {
		err = os.WriteFile(path, actual, 0644)
	} else {
		var expected []byte
		expected, err = os.ReadFile(path)
		if err == nil && !bytes.Equal(expected, actual) {
			err = fmt.Errorf("contract differs from %s", path)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
