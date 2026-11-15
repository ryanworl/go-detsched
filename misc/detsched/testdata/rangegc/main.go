// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Rangegc ranges over slices whose elements are 48 bytes while another
// goroutine runs the GC, and fails if a loop reads an element the GC
// freed. The compiler keeps only a uintptr to the element of such a
// range loop between iterations, so a preemption point on the loop's
// backedge, which detsched adds, must find the slice live; see
// walk.walkRange. Run it with GODEBUG=clobberfree=1, which fills freed
// memory, and a short quantum (GODETSEARCH="perturb=off quantum=3"), so
// loops are preempted often.
package main

import (
	"fmt"
	"os"
	"runtime"
)

type elem struct{ a, b, c, d, e, f uint64 } // 48 bytes

func main() {
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				runtime.GC()
			}
		}
	}()
	bad := 0
	for i := range 1000 {
		s := make([]elem, 2+i%2) // on the heap, as its length varies
		for j := range s {
			k := uint64(i*2 + j)
			s[j] = elem{k, k, k, k, k, k}
		}
		for j, v := range s {
			k := uint64(i*2 + j)
			if v != (elem{k, k, k, k, k, k}) {
				bad++
			}
		}
	}
	close(done)
	fmt.Println("freed elements read:", bad)
	if bad > 0 {
		os.Exit(1)
	}
}
