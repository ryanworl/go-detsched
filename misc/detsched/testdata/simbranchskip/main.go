// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Simbranchskip reaches a branch of the timeline while a pipe is
// registered with the network poller, so the process cannot fork it, and
// then a second branch, which fails. The second must still be branch 2,
// as GODETBRANCH counts it: a fork can also be refused for reasons that
// depend on real time, such as the watchdog having moved the P off the
// main thread, and before, a refused branch went uncounted, so the failing
// branch's GODETBRANCH setting replayed another branch.
package main

import (
	"fmt"
	"os"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func main() {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	if sim.Branch("the first branch passes") {
		os.Exit(0)
	}
	r.Close()
	w.Close()
	if sim.Branch("the second branch passes") {
		fmt.Println("in branch", sim.BranchNumber())
		os.Exit(1)
	}
	fmt.Println("timeline done")
}
