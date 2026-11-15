// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package nonidle blocks bubble goroutines in ways synctest does not count
// as durable: on a sync.Mutex, an RWMutex, and a channel created outside
// the bubble. Upstream, each of these tests hangs forever. Under detsched,
// once nothing anywhere can run, the bubble's clock jumps or Wait returns.
package nonidle

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// -test.parallel defaults to GOMAXPROCS, which is 1 under detsched. Raise
// it so the bubbles in TestParallelBubbles run at the same time.
func TestMain(m *testing.M) {
	flag.Set("test.parallel", "3")
	os.Exit(m.Run())
}

// A goroutine waits on a mutex held by a goroutine that sleeps.
func TestMutexHeldBySleeper(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		mu.Lock()
		go func() {
			time.Sleep(24 * time.Hour)
			mu.Unlock()
		}()
		mu.Lock()
		fmt.Println("mutex acquired after", time.Since(start))
		mu.Unlock()
	})
}

// Readers wait on an RWMutex while the writer sleeps between writes.
func TestRWMutex(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.RWMutex
		var log []string
		var wg sync.WaitGroup
		mu.Lock()
		for i := range 3 {
			wg.Go(func() {
				mu.RLock()
				log = append(log, fmt.Sprintf("r%d@%v", i, time.Since(start)))
				mu.RUnlock()
			})
		}
		time.Sleep(time.Minute)
		mu.Unlock()
		wg.Wait()
		fmt.Println("rwmutex:", log)
	})
}

// Wait returns while a goroutine is blocked on a mutex held by the test.
func TestWaitWithMutexWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		mu.Lock()
		acquired := false
		go func() {
			mu.Lock()
			acquired = true
			mu.Unlock()
		}()
		synctest.Wait()
		fmt.Println("after Wait, acquired =", acquired)
		mu.Unlock()
		synctest.Wait()
		fmt.Println("after Unlock and Wait, acquired =", acquired)
	})
}

// A channel created outside the bubble is not durable either.
var outside = make(chan time.Duration)

func TestChannelFromOutside(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		go func() {
			time.Sleep(90 * time.Minute)
			outside <- time.Since(start)
		}()
		fmt.Println("received", <-outside)
	})
}

// Parallel bubbles: the one whose next timer is earliest advances first.
func TestParallelBubbles(t *testing.T) {
	var mu sync.Mutex
	var order []string
	for i, d := range []time.Duration{3 * time.Hour, time.Second, 2 * time.Minute} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var bmu sync.Mutex
				bmu.Lock()
				go func() {
					time.Sleep(d)
					bmu.Unlock()
				}()
				bmu.Lock()
				mu.Lock()
				order = append(order, fmt.Sprint(d))
				mu.Unlock()
			})
		})
	}
	t.Cleanup(func() { fmt.Println("parallel bubbles finished in order", order) })
}
