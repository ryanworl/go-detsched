// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || dragonfly || freebsd || (js && wasm) || wasip1 || netbsd || openbsd || solaris

package os

// runtime_detReadDir reads directories in order only on Linux (see the
// Linux version).
func runtime_detReadDir(fd uintptr) (records []byte, errno uintptr, ok bool) {
	return nil, 0, false
}
