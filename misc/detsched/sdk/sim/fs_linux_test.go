// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

// preallocate makes d's file size bytes long with fallocate, as a log
// preallocates a segment on Linux.
func preallocate(w *World, d *fdesc, size int64) uintptr {
	_, e := w.fileCall(d, sysFallocate, [6]uintptr{0, 0, 0, uintptr(size)})
	return e
}
