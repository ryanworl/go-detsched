// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simlookup kills a node whose goroutines keep looking up a host
// name. Whatever the schedule, another node, and the node's next
// incarnation, must still be able to look the name up after the kill:
// net.DefaultResolver merges concurrent lookups of a name, and a lookup
// frozen in the middle would keep every later one waiting for good. The
// detsched test suite explores schedules.
package simlookup

import (
	"context"
	"net"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func lookup() {
	net.DefaultResolver.LookupIPAddr(context.Background(), "node-c") // the simulation knows no names
}

func TestKillLookup(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		b.Start(func() {})
		for i := range 30 {
			a.Start(func() {})
			for range 3 {
				a.Go(func() {
					for {
						lookup()
					}
				})
			}
			for range 10 + i {
				runtime.Gosched()
			}
			a.Kill()
			done := make(chan bool)
			b.Go(func() {
				lookup()
				done <- true
			})
			<-done
		}
	})
}
