// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Sdk uses the assertion SDK: after its setup, two goroutines race to
// increment a counter non-atomically, and an Always assertion checks the
// total, so it fails for some schedules.
package main

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
)

func main() {
	for range 20 {
		runtime.Gosched()
	}
	lifecycle.SetupComplete(nil)
	var n int
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			v := n
			runtime.Gosched()
			n = v + 1
		})
	}
	wg.Wait()
	assert.Sometimes(n == 2, "both increments counted", nil)
	assert.Always(n == 2, "no increment is lost", map[string]any{"n": n})
	assert.Reachable("end of main", nil)
	fmt.Println("n =", n)
}
