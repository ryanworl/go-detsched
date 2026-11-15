// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package runtime

import _ "unsafe" // for go:linkname

//go:linkname time_runtimeStartNano time.runtimeStartNano
func time_runtimeStartNano(n int64) {
	detsimStartNano = n
}
