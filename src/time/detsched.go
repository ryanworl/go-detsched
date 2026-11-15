// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched

package time

// runtimeStartNano tells the runtime startNano, so that the monotonic
// readings it gives times in a detsched simulation's bubble come out as
// the same offsets from startNano in every run; see runtime/detsim.go.
func runtimeStartNano(int64)

func init() {
	runtimeStartNano(startNano)
}
