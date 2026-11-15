// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Godebugkeys uses package internal/godebug through math/rand, which
// parses $GODEBUG at startup, and runs goroutines that allocate through
// many GC cycles, so that an allocation more or less at startup moves the
// schedule.
package main

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
)

type node struct {
	next *node
	id   int
	buf  [40]byte
}

var (
	mu   sync.Mutex
	runs []int // cleanup runs, in order
)

func worker(w int, wg *sync.WaitGroup) {
	defer wg.Done()
	var keep *node
	for i := range 100000 {
		n := &node{id: w*100000 + i, next: keep}
		if i%7 == 0 {
			keep = n // a growing list that stays live
		}
		if i%2000 == 0 {
			runtime.AddCleanup(n, func(id int) {
				mu.Lock()
				runs = append(runs, id)
				mu.Unlock()
			}, n.id)
		}
	}
	runtime.KeepAlive(keep)
}

func main() {
	rand.Int() // reads GODEBUG=randautoseed through internal/godebug
	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go worker(w, &wg)
	}
	wg.Wait()
	runtime.GC()
	runtime.GC()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("GCs %d, %d mallocs, %d cleanups ran\n", ms.NumGC, ms.Mallocs, len(runs))
}
