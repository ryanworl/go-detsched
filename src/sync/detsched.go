// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// runtime_detRLocked and runtime_detRUnlocked record that the calling
// goroutine holds an RWMutex, with the address of its w, for reading, for
// a simulation (GOEXPERIMENT=detsched); see runtime/detsim.go.
//
//go:linkname runtime_detRLocked
func runtime_detRLocked(w unsafe.Pointer)

//go:linkname runtime_detRUnlocked
func runtime_detRUnlocked(w unsafe.Pointer)
