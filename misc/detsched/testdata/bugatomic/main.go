// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Bugatomic has an atomicity violation with no call or loop inside it:
// setup checks an atomic flag and then sets it, so two goroutines can
// both see it unset and both run the setup. Ticks fall only at function
// entries and loop backedges, so only a yield at the synchronization
// point before the Store can split the check from the act.
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

var (
	done   atomic.Bool
	setups atomic.Int32
)

func setup() {
	if !done.Load() {
		done.Store(true)
		setups.Add(1)
	}
}

func main() {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(setup)
	}
	wg.Wait()
	if n := setups.Load(); n != 1 {
		panic(fmt.Sprint("setup ran ", n, " times"))
	}
	fmt.Println("ok")
}
