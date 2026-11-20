// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Simbranchstop reaches a branch of the timeline after each of eight
// rounds of work, and every branch works on for six more rounds and
// fails. A search runs it to a stop and forks runs from there; the
// stopped program must not fork the branches it passes, whose failures
// every run forked from it would report, and whose replay lines would
// apply the runs' branch points inside the branch.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// work runs a round: goroutines that yield to each other.
func work() {
	done := make(chan bool)
	for range 4 {
		go func() {
			for range 50 {
				runtime.Gosched()
			}
			done <- true
		}()
	}
	for range 4 {
		<-done
	}
}

func main() {
	for range 8 {
		work()
		if sim.Branch("every branch passes") {
			for range 6 {
				work()
			}
			fmt.Println("branch", sim.BranchNumber(), "fails")
			os.Exit(1)
		}
	}
	fmt.Println("timeline done")
}
