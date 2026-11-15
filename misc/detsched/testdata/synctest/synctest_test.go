// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package synctest runs goroutines with timers inside a synctest bubble,
// and sleeps outside the bubble, which today waits in real time.
package synctest

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestTimers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		var order []string
		var wg sync.WaitGroup
		for i, d := range []time.Duration{3 * time.Hour, time.Second, 2 * time.Minute} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(d)
				mu.Lock()
				order = append(order, fmt.Sprintf("%d@%v", i, time.Since(start)))
				mu.Unlock()
			}()
		}
		wg.Wait()
		fmt.Println("bubble:", order)
	})
	time.Sleep(50 * time.Millisecond)
	fmt.Println("slept outside the bubble")
}
