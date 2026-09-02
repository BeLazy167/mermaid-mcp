//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package render

import "os/exec"

func configureCommand(_ *exec.Cmd) {}
