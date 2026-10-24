// Command business-time checks physical-clock use in billing decision code.
package main

import (
	"fmt"
	"os"

	"github.com/open-rails/openrails/internal/businesstime"
)

func main() {
	root := "."
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "usage: business-time [repository-root]")
		os.Exit(2)
	}
	if len(os.Args) == 2 {
		root = os.Args[1]
	}
	if err := businesstime.Check(root, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
