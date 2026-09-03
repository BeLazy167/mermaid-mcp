//go:build linux

package main

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompDataArchOffset = 4
	seccompDataArgsOffset = 16
	x32SyscallBit         = 0x40000000
	socketTypeMask        = 0xf
)

func restrictNetwork() error {
	auditArch, err := nativeAuditArch()
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	// Fail the process on an unexpected ABI. Deny connect, io_uring, and IP datagrams.
	// Chromium uses a DevTools pipe, so renderer operation needs no network socket.
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArchOffset},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: auditArch},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: x32SyscallBit},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 8, K: unix.SYS_IO_URING_SETUP},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 7, K: unix.SYS_CONNECT},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 7, K: unix.SYS_SOCKET},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArgsOffset},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.AF_INET},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 4, K: unix.AF_INET6},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArgsOffset + 8},
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: socketTypeMask},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.SOCK_STREAM},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		return err
	}
	return nil
}

func nativeAuditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	default:
		return 0, errors.New("renderer network isolation supports amd64 and arm64 only")
	}
}
