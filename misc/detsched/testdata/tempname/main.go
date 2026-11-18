// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Tempname makes a temporary directory and file in $TEMPNAME_DIR and
// leaves them there, then runs goroutines whose schedule depends on the
// run's random numbers. A second run of its seed finds both names taken.
package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
)

func main() {
	parent := os.Getenv("TEMPNAME_DIR")
	dir, err := os.MkdirTemp(parent, "dir")
	if err != nil {
		panic(err)
	}
	f, err := os.CreateTemp(parent, "file*.log")
	if err != nil {
		panic(err)
	}
	f.Close()
	fmt.Println(dir)
	fmt.Println(f.Name())
	var mu sync.Mutex
	m := map[int]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				mu.Lock()
				m[rand.IntN(50)]++
				mu.Unlock()
				if rand.IntN(2) == 0 {
					runtime.Gosched()
				}
			}
		})
	}
	wg.Wait()
}
