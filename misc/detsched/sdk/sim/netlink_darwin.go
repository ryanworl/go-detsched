// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

import "syscall"

// Darwin has no netlink sockets.

const afNetlink = 1 << 16 // no address family

func (w *World) netlinkSocket(*incarnation, [6]uintptr) (*socket, uintptr) {
	return nil, uintptr(syscall.EAFNOSUPPORT)
}

func (w *World) netlinkCall(*fdesc, uintptr, [6]uintptr) (uintptr, uintptr) {
	return 0, uintptr(syscall.EOPNOTSUPP)
}
