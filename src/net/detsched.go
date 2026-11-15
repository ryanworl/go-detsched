// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package net

import _ "unsafe" // for go:linkname

// runtime_detsimNode returns the simulated node of the calling goroutine
// under GOEXPERIMENT=detsched, or 0; see runtime/detsim.go. It is pushed
// by the runtime.
//
//go:linkname runtime_detsimNode
func runtime_detsimNode() int32
