// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simstderr kills a node whose goroutines keep writing to
// os.Stderr. Whatever the schedule, another node must still be able to
// write to it after the kill: a goroutine frozen while it held the write
// lock of os.Stderr's poll.FD would block every later write for good. The
// detsched test suite explores schedules.
package simstderr

import (
	"os"
	"runtime"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestKillWriter(t *testing.T) {
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
						os.Stderr.Write(nil) // takes the lock, writes nothing
					}
				})
			}
			for range 10 + i {
				runtime.Gosched()
			}
			a.Kill()
			done := make(chan bool)
			b.Go(func() {
				os.Stderr.Write(nil)
				done <- true
			})
			<-done
		}
	})
}
