// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// detAuxv returns the value of auxiliary vector entry tag that det mode
// gives the process instead of val: on arm64, AT_HWCAP with only the
// bits of detHWCapKeep, and AT_HWCAP2 zero, so that code that chooses by
// them, in the standard library or not, takes the same paths on every
// machine (see detmachine.go). Called by sysauxv, before anything reads
// the vector.
func detAuxv(tag, val uintptr) uintptr {
	if GOARCH != "arm64" {
		return val
	}
	switch tag {
	case _AT_HWCAP:
		detHWCap.set, detHWCap.hwcap = true, val
		return val & detHWCapKeep
	case _AT_HWCAP2:
		detHWCap.hwcap2 = val
		return 0
	}
	return val
}
