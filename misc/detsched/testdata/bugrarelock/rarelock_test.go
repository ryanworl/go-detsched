// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bugrarelock has a lock-order deadlock that random schedules
// rarely reach (about 1 in 70 seeds of the strategy swarm). A worker takes
// a then b once, after a little work; a monitor takes b then a once, after
// much more. They deadlock only if one is preempted between its two Locks
// and the other runs into its own pair before it resumes.
package bugrarelock

import (
	"sync"
	"testing"
	"testing/synctest"
)

func TestRebalance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var a, b sync.Mutex
		var wg sync.WaitGroup
		n := 0
		wg.Go(func() {
			work(2000)
			a.Lock()
			b.Lock()
			n++
			b.Unlock()
			a.Unlock()
		})
		wg.Go(func() {
			work(30000)
			b.Lock()
			a.Lock()
			n++
			a.Unlock()
			b.Unlock()
		})
		wg.Wait()
	})
}

//go:noinline
func work(k int) int {
	n := 0
	for i := range k {
		n += i
	}
	return n
}
