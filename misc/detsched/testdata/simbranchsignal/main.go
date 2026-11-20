// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Simbranchsignal forks a branch of the timeline that recovers from a nil
// dereference and passes, then ignores SIGCHLD, so that the next branch
// cannot be waited for. On macOS a forked child loses its thread's
// signal stack, and before, the first branch's SIGSEGV was handled on the
// goroutine's stack, where the handler waited for good: the branch, and
// the timeline waiting for it, hung, though the branch passes in place.
// The second branch passes too, but wait4 fails with ECHILD, and before,
// the timeline reported that the branch exited with status 1.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var nilp *int

func main() {
	if sim.Branch("the first branch passes") {
		func() {
			defer func() { recover() }()
			fmt.Println(*nilp)
		}()
		os.Exit(0)
	}
	signal.Ignore(syscall.SIGCHLD)
	if sim.Branch("the second branch passes") {
		os.Exit(0)
	}
	fmt.Println("timeline done")
}
