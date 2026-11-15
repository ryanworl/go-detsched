// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Gcstress runs goroutines that allocate concurrently through many GC
// cycles, using cleanups, finalizers, weak pointers, and sync.Pool, and
// prints digests of everything the GC's timing could affect.
package main

import (
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"sync"
	"unsafe"
	"weak"
)

type node struct {
	next *node
	id   int
	buf  [40]byte
}

var (
	mu      sync.Mutex
	events  []string // cleanup and finalizer runs, in order
	pool    = sync.Pool{New: func() any { return new([256]byte) }}
	addrSum = fnv.New64a()
)

func record(e string) {
	mu.Lock()
	events = append(events, e)
	mu.Unlock()
}

func worker(w int, wg *sync.WaitGroup, weaks chan<- weak.Pointer[node]) {
	defer wg.Done()
	os.Getenv("GCSTRESS") // the first lookup scans the whole environment
	var keep *node
	for i := range 100000 {
		n := &node{id: w*100000 + i, next: keep}
		if i%7 == 0 {
			keep = n // a growing list that stays live
		}
		if i%2000 == 0 {
			runtime.AddCleanup(n, func(id int) { record(fmt.Sprint("c", id)) }, n.id)
			weaks <- weak.Make(n)
		}
		if i%5000 == 1 {
			runtime.SetFinalizer(n, func(n *node) { record(fmt.Sprint("f", n.id)) })
		}
		b := pool.Get().(*[256]byte)
		b[0] = byte(i)
		pool.Put(b)
		if i%997 == 0 {
			mu.Lock()
			p := uintptr(unsafe.Pointer(n))
			addrSum.Write(unsafe.Slice((*byte)(unsafe.Pointer(&p)), unsafe.Sizeof(p)))
			mu.Unlock()
		}
	}
	runtime.KeepAlive(keep)
}

func main() {
	var wg sync.WaitGroup
	weaks := make(chan weak.Pointer[node], 1000)
	for w := range 6 {
		wg.Add(1)
		go worker(w, &wg, weaks)
	}
	wg.Wait()
	close(weaks)
	runtime.GC()
	runtime.GC()

	alive := 0
	for wp := range weaks {
		if wp.Value() != nil {
			alive++
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	mu.Lock()
	defer mu.Unlock()
	h := fnv.New64a()
	for _, e := range events {
		h.Write([]byte(e))
	}
	fmt.Printf("GCs %d, heap %d bytes, %d mallocs\n", ms.NumGC, ms.HeapAlloc, ms.Mallocs)
	fmt.Printf("%d cleanups and finalizers ran, order %x\n", len(events), h.Sum64())
	fmt.Printf("%d weak pointers still alive\n", alive)
	fmt.Printf("addresses %x\n", addrSum.Sum64())
}
