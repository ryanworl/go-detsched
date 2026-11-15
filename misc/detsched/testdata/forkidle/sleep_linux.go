// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"syscall"
	"unsafe"
)

// sleep30ms sleeps in a system call for 30ms.
func sleep30ms() {
	ts := syscall.Timespec{Nsec: 30e6}
	syscall.Syscall(syscall.SYS_NANOSLEEP, uintptr(unsafe.Pointer(&ts)), 0, 0)
}
