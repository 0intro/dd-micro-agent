//go:build !openbsd && !netbsd

package main

import (
	"fmt"
	"os"
)

// proccap reads the OpenBSD/NetBSD process sysctl, so on every other platform it is
// just a stub that keeps "go build ./..." green.
func main() {
	fmt.Fprintln(os.Stderr, "proccap supports openbsd and netbsd only")
	os.Exit(1)
}
