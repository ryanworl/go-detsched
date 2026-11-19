// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package maps

import (
	"internal/abi"
	"unsafe"
)

// DetAssign is set while a map hashes a key it is about to store, with
// GOEXPERIMENT=detsched, where a pointer key hashes by its identity
// (see runtime/detptrid.go): only a stored pointer gets one, so that
// looking up a key that is not there, which the hash of its address
// answers as well, leaves the identities that later keys get alone.
var DetAssign bool

// detAssignHash is typ.Hasher(key, seed) for a key the caller stores.
func detAssignHash(typ *abi.MapType, key unsafe.Pointer, seed uintptr) uintptr {
	DetAssign = true
	h := typ.Hasher(key, seed)
	DetAssign = false
	return h
}
