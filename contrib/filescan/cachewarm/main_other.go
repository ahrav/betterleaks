//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "filescan-cachewarm requires Linux cachestat, fadvise, and mincore")
	os.Exit(1)
}
