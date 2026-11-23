// Package demo holds the fixture's regions.
package demo

import "fmt"

// Hello is the guide's first snippet.
func Hello() {
	// guide:demo-hello
	fmt.Println("hello")
	// guide:hide
	fmt.Println("not in the guide")
	// guide:show
	fmt.Println("bye")
	// guide:end
}
