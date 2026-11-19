// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// runtime_detLocked and runtime_detUnlocked record mutex operations for
// det-mode feedback (GOEXPERIMENT=detsched); see runtime/detfeed.go.
//
//go:linkname runtime_detLocked
func runtime_detLocked(m unsafe.Pointer)

//go:linkname runtime_detUnlocked
func runtime_detUnlocked(m unsafe.Pointer)

// runtime_detHashAssign is hash(key, seed) for a key the caller stores,
// with GOEXPERIMENT=detsched, where only such a key gives the pointers in
// it identities to hash by (see runtime/detptrid.go).
//
//go:linkname runtime_detHashAssign
//go:noescape
func runtime_detHashAssign(hash func(unsafe.Pointer, uintptr) uintptr, key unsafe.Pointer, seed uintptr) uintptr
