// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.detsched

package buggify

const supported = false

func runtime_rand() uint64  { return 0 }
func runtime_epoch() uint64 { return 0 }
func runtime_call(f func()) { f() }

func runtime_faultOff(string) bool { return false }
