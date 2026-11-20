// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Pidsched's schedule depends on its process ID, which the runtime does
// not make deterministic, so that two runs of a seed, which have
// different IDs, run different schedules: a goroutine yields as many
// times as the ID's last three digits, plus 10, while another counts.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	done := make(chan int)
	go func() {
		n := 0
		for range 2000 {
			n++
			runtime.Gosched()
		}
		done <- n
	}()
	for range os.Getpid()%1000 + 10 {
		runtime.Gosched()
	}
	fmt.Println(<-done)
}
