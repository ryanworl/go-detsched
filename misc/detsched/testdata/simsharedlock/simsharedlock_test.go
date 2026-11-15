// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simsharedlock shares a mutex between two simulated nodes, as
// the global state of a library does: node a crashes while it holds the
// mutex and sleeps, and node b then waits for it. The goroutine of a runs
// on as a zombie when its sleep ends, until it releases the mutex, and b
// takes it.
package simsharedlock

import (
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var shared sync.Mutex

func TestSharedLock(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			shared.Lock()
			time.Sleep(time.Hour)
			shared.Unlock()
		})
		time.Sleep(time.Second)
		a.Kill()
		done := make(chan bool)
		b.Start(func() {
			shared.Lock()
			fmt.Println("b took the lock")
			shared.Unlock()
			done <- true
		})
		<-done
	})
}
