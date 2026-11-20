// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Simbranchlimit, with any seed but 1, fails an assertion in the first
// branch of its timeline, and the second branch then runs long without
// moving virtual time, which a search stops at its livelock limit. The
// failure the search reports is the first branch's, which comes first,
// and its replay line must run that branch, on that branch's schedule,
// though the second branch's report, which the timeline writes after its
// own, comes last.
package main

import (
	"fmt"
	"os"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func main() {
	fail := os.Getenv("GODETSEED") != "1"
	if sim.Branch("the first branch passes") {
		assert.Always(!fail, "the first branch's check holds", nil)
		return
	}
	if sim.Branch("the second branch passes") {
		if fail {
			// Passes, unless stopped at a livelock limit.
			ch := make(chan int)
			go func() {
				for range 500000 {
					ch <- 1
				}
				close(ch)
			}()
			for range ch {
			}
		}
		return
	}
	fmt.Println("timeline done")
}
