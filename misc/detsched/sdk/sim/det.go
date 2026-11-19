// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.detsched && ((linux && (amd64 || arm64)) || (darwin && !ios && arm64))

package sim

import "unsafe"

const supported = true

func register() {
	runtime_register(handleCharged, timerFired, rawTraps)
	runtime_onFork(forked)
	runtime_failLog(&timeline)
}

// handleCharged handles a system call as handle does, and charges the
// node's goroutine the CPU time the call would take: a fixed cost, plus
// a cost per byte it copied. The time the device takes, such as an
// fsync's or a message's latency, is not CPU time.
func handleCharged(node int32, trap uintptr, a [6]uintptr) (r1, r2, errno uintptr, ok bool) {
	r1, r2, errno, ok = handle(node, trap, a)
	if ok && node != 0 {
		runtime_busyAdd(syscallCostNs + int64(copied(trap, r1, errno))*syscallBytePs/1000)
	}
	return
}

// unregister stops routing system calls to the simulation. Its events
// still pending fire later, if the bubble goes on, and timerFired, which
// stays registered, ignores them.
func unregister() {
	runtime_register(nil, timerFired, [4]uintptr{})
	runtime_onFork(nil)
}

// The runtime provides these (runtime/detsim.go).

//go:linkname runtime_register
func runtime_register(handler func(int32, uintptr, [6]uintptr) (uintptr, uintptr, uintptr, bool), timer func(uint64), rawTraps [4]uintptr)

//go:linkname runtime_onFork
func runtime_onFork(f func())

//go:linkname runtime_node
func runtime_node() int32

//go:linkname runtime_setNode
func runtime_setNode(node int32) int32

// runtime_call runs f at once, on the system stack.
//
//go:linkname runtime_faultOff
func runtime_faultOff(group, name string) bool

//go:linkname runtime_call
//go:noescape
func runtime_call(f func())

//go:linkname runtime_crash
func runtime_crash(node int32)

//go:linkname runtime_halt
func runtime_halt()

//go:linkname runtime_after
func runtime_after(delay int64, id uint64)

//go:linkname runtime_now
func runtime_now() int64

//go:linkname runtime_ready
func runtime_ready(fd uintptr, mode int32)

//go:linkname runtime_rand
func runtime_rand() uint64

//go:linkname runtime_epoch
func runtime_epoch() uint64

//go:linkname runtime_branch
func runtime_branch(msg string) int

//go:linkname runtime_pause
func runtime_pause(node int32)

//go:linkname runtime_resume
func runtime_resume(node int32)

//go:linkname runtime_count
func runtime_count(node int32) int

//go:linkname runtime_stacks
func runtime_stacks(node int32, buf []byte) int

//go:linkname runtime_clock
func runtime_clock(on bool, skew int64)

//go:linkname runtime_onPanic
func runtime_onPanic(f func(node int32, v any))

//go:linkname runtime_busyOn
func runtime_busyOn(on bool)

//go:linkname runtime_setCPU
func runtime_setCPU(node int32, cores, speed int32)

//go:linkname runtime_nodeName
func runtime_nodeName(node int32, name string)

//go:linkname runtime_busyAdd
func runtime_busyAdd(ns int64)

//go:linkname runtime_wait
func runtime_wait(ns int64)

//go:linkname runtime_wake
func runtime_wake(node int32)

//go:linkname runtime_busyStats
func runtime_busyStats() (ticks, parks uint64, paidNs, streakNs int64, streakNode int32)

//go:linkname runtime_watch
func runtime_watch(calls *uint32, node func(unsafe.Pointer, int32) (string, bool))

//go:linkname runtime_failLog
func runtime_failLog(p *[]byte)
