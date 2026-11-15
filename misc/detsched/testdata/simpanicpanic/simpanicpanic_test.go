// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simpanicpanic panics in Config.OnPanic. The runtime calls the
// handler once the node's panicking goroutine has run all its deferred
// calls, so the handler's panic starts after the first one finished
// unwinding. It must end the program with both panics, as a panic in a
// deferred call does, instead of losing its way on the stack ("fatal
// error: unknown pc").
package simpanicpanic

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestPanicInOnPanic(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{OnPanic: func(*sim.Node, string, []byte) {
			panic("in OnPanic")
		}})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		a.Start(func() {
			defer func() {}()
			panic("on the node")
		})
		time.Sleep(time.Second)
	})
}
