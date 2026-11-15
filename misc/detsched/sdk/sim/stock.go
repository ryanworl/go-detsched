// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.detsched && ((linux && (amd64 || arm64)) || (darwin && !ios && arm64))

package sim

import "unsafe"

const supported = false

func register()   {}
func unregister() {}

func runtime_node() int32                { return 0 }
func runtime_setNode(int32) int32        { return 0 }
func runtime_call(f func())              { f() }
func runtime_crash(int32)                {}
func runtime_halt()                      { select {} }
func runtime_after(int64, uint64)        {}
func runtime_now() int64                 { return 0 }
func runtime_ready(uintptr, int32)       {}
func runtime_rand() uint64               { return 0 }
func runtime_epoch() uint64              { return 0 }
func runtime_branch(string) int          { return 0 }
func runtime_pause(int32)                {}
func runtime_resume(int32)               {}
func runtime_count(int32) int            { return 0 }
func runtime_stacks(int32, []byte) int   { return 0 }
func runtime_clock(bool, int64)          {}
func runtime_onPanic(func(int32, any))   {}
func runtime_busyOn(bool)                {}
func runtime_setCPU(int32, int32, int32) {}
func runtime_busyAdd(int64)              {}
func runtime_wait(int64)                 {}
func runtime_wake(int32)                 {}
func runtime_busyStats() (uint64, uint64, int64, int64, int32) {
	return 0, 0, 0, 0, 0
}

func runtime_watch(*uint32, func(unsafe.Pointer, int32) (string, bool)) {}
