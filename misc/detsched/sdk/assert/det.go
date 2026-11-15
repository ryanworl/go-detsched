// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package assert

import _ "unsafe" // for go:linkname

func emit(kind int, cond, failed bool, message, details string) {
	runtime_assert(kind, cond, message, details)
}

// runtime_assert is provided by the runtime (runtime/detfeed.go).
//
//go:linkname runtime_assert
func runtime_assert(kind int, cond bool, message, details string)
