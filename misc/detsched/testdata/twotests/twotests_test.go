// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package twotests has two tests that each set up a simulation, which
// one process runs one after the other: only the first test's setup is a
// fork point, so the summary must say to search each test on its own
// (TestSearchTwoTests).
package twotests

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func run(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		<-n.Start(func() {
			time.Sleep(time.Second) // setup
			lifecycle.SetupComplete(nil)
			time.Sleep(time.Second)
		})
	})
}

func TestFirst(t *testing.T)  { run(t) }
func TestSecond(t *testing.T) { run(t) }
