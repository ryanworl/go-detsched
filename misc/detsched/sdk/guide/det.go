// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package guide

import _ "unsafe" // for go:linkname

func state(name string, s uint64)   { runtime_state(name, s) }
func maximize(name string, v int64) { runtime_maximize(name, v) }

// runtime_state and runtime_maximize are provided by the runtime
// (runtime/detfeed.go).
//
//go:linkname runtime_state
func runtime_state(name string, s uint64)

//go:linkname runtime_maximize
func runtime_maximize(name string, v int64)
