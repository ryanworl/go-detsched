// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

package main

import "syscall"

// controlSockets returns a SOCK_STREAM Unix socket pair, for a fork
// server's control socket. macOS has no SOCK_SEQPACKET Unix sockets; each
// command and reply is 32 bytes, which a stream socket delivers whole, and
// the end of the program reads as the end of the stream.
func controlSockets() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}

// memory returns how many bytes of memory are available to programs, and
// how many there are, or zeros if it cannot tell.
func memory() (avail, total uint64) {
	level, err := syscall.SysctlUint32("kern.memorystatus_level") // percent available
	if err != nil {
		return 0, 0
	}
	b, err := syscall.Sysctl("hw.memsize") // little-endian, less a trailing 0 byte
	if err != nil {
		return 0, 0
	}
	for i := len(b) - 1; i >= 0; i-- {
		total = total<<8 | uint64(b[i])
	}
	return total / 100 * uint64(level), total
}

// lowerPriority lowers our scheduling priority to nice 10, which the
// programs we run inherit, so that a search that keeps every CPU busy
// leaves the CPUs to the user's other work when it wants them.
func lowerPriority() {
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
}
