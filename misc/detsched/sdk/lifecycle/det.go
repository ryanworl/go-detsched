// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package lifecycle

import _ "unsafe" // for go:linkname

func setupComplete() { runtime_setupComplete() }

// runtime_setupComplete is provided by the runtime (runtime/detsched.go).
//
//go:linkname runtime_setupComplete
func runtime_setupComplete()

func branches() int { return runtime_branches() }

// runtime_branches is provided by the runtime (runtime/detsched.go).
//
//go:linkname runtime_branches
func runtime_branches() int
