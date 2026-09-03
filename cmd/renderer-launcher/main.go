package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: renderer-launcher executable [arguments...]")
		os.Exit(2)
	}
	if err := restrictNetwork(); err != nil {
		fmt.Fprintf(os.Stderr, "apply renderer network isolation: %v\n", err)
		os.Exit(1)
	}
	if err := unix.Exec(os.Args[1], os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "start renderer: %v\n", err)
		os.Exit(1)
	}
}
