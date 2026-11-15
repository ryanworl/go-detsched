// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Mutexstarve has goroutines contend for a sync.Mutex, each holding it for
// about a millisecond and relocking it soon after unlocking, so a woken
// waiter often finds it locked again. It prints a digest of the order in
// which they acquired it. Upstream, a waiter switches the mutex to
// starvation mode after waiting 1 ms of wall-clock time, so the order
// depends on machine speed.
package main

import (
	"fmt"
	"hash/fnv"
	"sync"
)

const (
	workers = 3
	rounds  = 300
	hold    = 1 << 18 // loop iterations while holding the lock
)

var (
	mu    sync.Mutex
	order []byte
	sink  int
)

func main() {
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			x := 0
			for range rounds {
				mu.Lock()
				order = append(order, byte('A'+w))
				for i := range hold {
					x += i ^ w
				}
				mu.Unlock()
				for i := range hold / 64 {
					x += i
				}
			}
			mu.Lock()
			sink += x
			mu.Unlock()
		})
	}
	wg.Wait()

	h := fnv.New64a()
	h.Write(order)
	switches := 0
	for i := 1; i < len(order); i++ {
		if order[i] != order[i-1] {
			switches++
		}
	}
	fmt.Printf("acquisitions=%d switches=%d order=%#x\n", len(order), switches, h.Sum64())
}
