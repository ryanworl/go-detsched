// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Spinwait has goroutines that spin, with runtime.Gosched and with bare
// atomic loads, until another goroutine sets a flag. Under PCT's strict
// priorities a spinner with the higher priority would run forever; the
// budget of quantum ticks per priority makes it give way.
package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

func main() {
	var yielding, bare atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !yielding.Load() {
			runtime.Gosched()
		}
	})
	wg.Go(func() {
		for !bare.Load() {
		}
	})
	wg.Go(func() { yielding.Store(true) })
	wg.Go(func() { bare.Store(true) })
	wg.Wait()
	fmt.Println("ok")
}
