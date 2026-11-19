// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package badharness

import (
	"net"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// TestFatalOnNode calls t.Fatal on a simulated node's goroutine, where a
// dial to a node that does not listen fails, and waits for the goroutine,
// as simulation harnesses do. t.Fatal ends only that goroutine, so the
// test would wait for good: the run must fail at the call. go vet sees no
// goroutine here, as Node.Go starts it.
func TestFatalOnNode(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.Start(func() {})
		b.Start(func() {})
		done := make(chan bool)
		a.Go(func() {
			c, err := net.Dial("tcp", "10.0.0.2:7")
			if err != nil {
				t.Fatal(err)
			}
			c.Close()
			done <- true
		})
		<-done
	})
}
