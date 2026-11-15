// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forkidle leaves an idle M behind and later needs it. Each phase blocks
// the main goroutine in 30ms system calls while another goroutine is
// runnable, so the det watchdog hands the P to a second M; when that M runs
// out of work it parks, and the main goroutine takes the P back. A process
// forked between the phases must give the parked M a new thread for the
// second hand-off. It is outside the deterministic subset, since the
// hand-off depends on real time; it exists to test fork mode.
package main

import (
	"fmt"
	"runtime"
	"sync/atomic"
)

func phase(name string) {
	var ran atomic.Bool
	done := make(chan bool)
	go func() { ran.Store(true); done <- true }()
	// The goroutine runs during a system call only if the P is handed
	// off. On a loaded machine the watchdog may be late, so try again.
	for i := 0; i < 100 && !ran.Load(); i++ {
		sleep30ms()
	}
	fmt.Println(name, "handed off:", ran.Load())
	<-done
}

func main() {
	phase("phase 1")
	for range 100 {
		runtime.Gosched()
	}
	phase("phase 2")
}
