// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package buglockorder has a lock-order deadlock: two goroutines take the
// same two mutexes in opposite orders. Without perturbation the first
// goroutine takes and releases both before the second starts; a perturbed
// schedule can preempt it in between, and synctest reports the deadlock.
package buglockorder

import (
	"sync"
	"testing"
	"testing/synctest"
)

func TestTransfer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var a, b sync.Mutex
		var wg sync.WaitGroup
		wg.Go(func() { lockBoth(&a, &b) })
		wg.Go(func() { lockBoth(&b, &a) })
		wg.Wait()
	})
}

func lockBoth(first, second *sync.Mutex) {
	first.Lock()
	defer first.Unlock()
	work()
	second.Lock()
	defer second.Unlock()
	work()
}

func work() int {
	n := 0
	for i := range 100 {
		n += i
	}
	return n
}
