//go:build linux

package main

import (
	"fmt"
	"syscall"
)

const prSetDumpable = 4

func protectProcessSecrets() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0)
	if errno != 0 {
		return fmt.Errorf("disable process dumpability: %w", errno)
	}
	return nil
}
