// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simfinalizer drops objects with finalizers and cleanups on a
// node of a World that always has a goroutine to run. The finalizers and
// cleanups must still run. Under PCT perturbation the scheduler picked
// a runnable goroutine before it looked at the finalizer goroutine, so
// with GODETSEED=3 none ran. TestSimPrograms runs it.
package simfinalizer

import (
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

type obj struct{ b [4096]byte }

func TestFinalizer(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	var made, finalized, cleaned atomic.Int64
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		done := make(chan bool)
		n.Start(func() {
			go func() {
				for {
					time.Sleep(time.Millisecond)
				}
			}()
			for i := range 100000 {
				o := new(obj)
				o.b[0] = byte(i)
				made.Add(1)
				if i%2 == 0 {
					runtime.SetFinalizer(o, func(*obj) { finalized.Add(1) })
				} else {
					runtime.AddCleanup(o, func(int) { cleaned.Add(1) }, i)
				}
			}
			done <- true
		})
		<-done
	})
	// About 400 MB went through a small heap: all but the last few
	// collections' worth must have been finalized and cleaned up.
	if f, c := finalized.Load(), cleaned.Load(); f < made.Load()/4 || c < made.Load()/4 {
		t.Errorf("of %d objects, %d finalized and %d cleaned up, want most", made.Load(), f, c)
	}
}
