// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "syscall"

// sleep30ms sleeps in a system call for 30ms.
func sleep30ms() {
	tv := syscall.Timeval{Usec: 30e3}
	syscall.Select(0, nil, nil, nil, &tv)
}
