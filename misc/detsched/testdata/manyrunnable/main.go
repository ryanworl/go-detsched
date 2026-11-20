// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Manyrunnable makes 30000 goroutines runnable at once, by closing a
// channel they all wait for, and has each yield a few times before it
// exits, so that every switch picks among tens of thousands.
package main

import (
	"fmt"
	"runtime"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	sum := 0
	for i := range 30000 {
		wg.Go(func() {
			<-start
			for range 4 {
				runtime.Gosched()
			}
			mu.Lock()
			sum += i
			mu.Unlock()
		})
	}
	close(start)
	wg.Wait()
	fmt.Println(sum)
}
