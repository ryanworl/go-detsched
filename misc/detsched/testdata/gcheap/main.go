// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Gcheap allocates enough to run the garbage collector several times and
// prints the order in which cleanups ran, the iteration order of a map
// keyed by heap pointers, and some heap addresses.
package main

import (
	"fmt"
	"runtime"
	"sync"
)

type node struct {
	id   int
	next *node
	pad  [48]byte
}

var (
	mu  sync.Mutex
	ran []int
)

func main() {
	var keep []*node
	for i := range 20000 {
		n := &node{id: i}
		if i%1000 == 0 {
			runtime.AddCleanup(n, func(id int) {
				mu.Lock()
				ran = append(ran, id)
				mu.Unlock()
			}, i)
		}
		if i%97 == 0 {
			keep = append(keep, n)
		}
		_ = make([]byte, 1024) // garbage
	}
	runtime.GC()
	runtime.GC()

	m := map[*node]int{}
	for _, n := range keep[:16] {
		m[n] = n.id
	}
	fmt.Print("pointer map order:")
	for _, id := range m {
		fmt.Print(" ", id)
	}
	fmt.Println()
	fmt.Printf("addresses: %p %p\n", keep[0], keep[len(keep)-1])

	mu.Lock()
	fmt.Println("cleanups ran:", ran)
	mu.Unlock()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Println("GCs:", ms.NumGC)
}
