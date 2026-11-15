// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simdeadticker kills a node whose goroutine reads a time.Ticker.
// A crash ends the process's timers, so the ticker must stop. If it went
// on, the bubble's clock would jump from tick to tick whenever every live
// goroutine is blocked: the hour's sleep below would take a scheduler
// round per microsecond tick, and a deadlock of the live nodes would spin
// forever instead of failing.
package simdeadticker

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestKillTicker(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		a.Start(func() {})
		ticking := make(chan bool)
		a.Go(func() {
			tk := time.NewTicker(time.Microsecond)
			<-tk.C
			close(ticking)
			for range tk.C {
			}
		})
		<-ticking
		a.Kill()
		time.Sleep(time.Hour)
	})
}
