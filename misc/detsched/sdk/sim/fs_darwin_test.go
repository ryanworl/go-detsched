// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

import (
	"syscall"
	"unsafe"
)

// preallocate makes d's file size bytes long with fcntl(F_PREALLOCATE)
// and ftruncate, as a log preallocates a segment on macOS.
func preallocate(w *World, d *fdesc, size int64) uintptr {
	fs := syscall.Fstore_t{Flags: syscall.F_ALLOCATEALL, Posmode: syscall.F_PEOFPOSMODE, Length: size}
	if _, e := w.fileCall(d, sysFcntl, [6]uintptr{0, syscall.F_PREALLOCATE, uintptr(unsafe.Pointer(&fs))}); e != 0 {
		return e
	}
	if fs.Bytesalloc != size {
		return uintptr(syscall.EIO)
	}
	_, e := w.fileCall(d, sysFtruncate, [6]uintptr{0, uintptr(size)})
	return e
}
