// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package buggify

import _ "unsafe" // for go:linkname

const supported = true

// The runtime provides these (runtime/detsim.go).

//go:linkname runtime_rand
func runtime_rand() uint64

//go:linkname runtime_epoch
func runtime_epoch() uint64
