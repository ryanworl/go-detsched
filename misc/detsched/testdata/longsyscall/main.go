// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Longsyscall makes system calls that take far longer than the
// watchdog's 10ms but wait for nothing: reads of hundreds of megabytes
// from /dev/urandom, in which the thread runs in the kernel the whole time,
// as a thread that a loaded machine leaves without a CPU waits for one.
// Other goroutines count meanwhile. The watchdog must not hand off the P
// for them, which would let the others run during the read and make the
// schedule depend on how long the read took.
package main

import (
	"fmt"
	"os"
	"sync"
)

func main() {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		panic(err)
	}
	buf := make([]byte, 128<<20)
	var mu sync.Mutex
	var wg sync.WaitGroup
	n := 0
	for i := range 3 {
		wg.Go(func() {
			for j := range 20000 {
				mu.Lock()
				n += i ^ j
				mu.Unlock()
			}
		})
	}
	read := 0
	for range 3 {
		m, err := f.Read(buf)
		if err != nil {
			panic(err)
		}
		read += m
	}
	wg.Wait()
	fmt.Println(n, read)
}
