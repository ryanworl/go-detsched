// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gcidle waits for finalizers and cleanups without calling
// runtime.GC, which under detsched run only at a GC epoch: the runtime
// must start one when no goroutine can run, rather than let the program
// wait for good or report a deadlock.
package gcidle

import (
	"fmt"
	"runtime"
	"testing"
	"testing/synctest"
	"time"
	"weak"
)

type obj struct {
	p *int // a pointer, so the object is not in a tiny block
	n [8]int
}

// TestCleanupInBubble: the only goroutine of a bubble waits on a cleanup.
// Cleanups run outside the bubble, so the channel is made outside it.
func TestCleanupInBubble(t *testing.T) {
	done := make(chan int)
	synctest.Test(t, func(t *testing.T) {
		runtime.AddCleanup(new(obj), func(v int) { done <- v }, 7)
		fmt.Println("bubble cleanup:", <-done)
	})
}

// TestFinalizerOutsideBubble: the only goroutine of the program waits on a
// finalizer, where the runtime would report that all goroutines are
// asleep.
func TestFinalizerOutsideBubble(t *testing.T) {
	done := make(chan string)
	o := new(obj)
	runtime.SetFinalizer(o, func(*obj) { done <- "ran" })
	o = nil
	fmt.Println("finalizer:", <-done)
}

// TestDroppedAfterEpoch: an epoch at an earlier idle point found the
// object alive; dropped afterwards, its cleanup runs at the next one, and
// a weak pointer to it reads nil.
func TestDroppedAfterEpoch(t *testing.T) {
	keptDone, first := make(chan bool), make(chan bool)
	synctest.Test(t, func(t *testing.T) {
		kept := new(obj)
		w := weak.Make(kept)
		runtime.AddCleanup(kept, func(bool) { keptDone <- true }, true)
		runtime.AddCleanup(new(obj), func(bool) { first <- true }, true)
		<-first
		time.Sleep(time.Second) // the bubble's clock jumps: no epoch
		fmt.Println("kept alive through an epoch:", w.Value() == kept)
		runtime.KeepAlive(kept)
		kept = nil
		<-keptDone
		fmt.Println("dropped, then cleaned up; weak pointer nil:", w.Value() == nil)
	})
}
