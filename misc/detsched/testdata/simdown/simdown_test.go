// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simdown waits on Node.Down for nodes that end every way a node
// ends: a panic OnPanic catches, Kill, KillProcess, and Stop, and checks
// that Down is closed for a node that is down or was never started. It
// also waits on the channel Start returns, which closes when the start
// function returns, with the node still up, or when the node crashes in
// the middle of it.
package simdown

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestDown(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{OnPanic: func(*sim.Node, string, []byte) {}})
		defer w.Close()
		n := w.AddNode("n", "10.0.0.1")
		<-n.Down() // never started
		block := func() { select {} }
		for _, end := range []string{"panic", "kill", "killprocess", "stop"} {
			var done <-chan struct{}
			if end == "panic" {
				done = n.Start(func() {
					time.Sleep(time.Second)
					panic("on the node")
				})
			} else {
				done = n.Start(block)
			}
			down := n.Down()
			select {
			case <-down:
				t.Fatalf("%s: Down closed while the node is up", end)
			default:
			}
			switch end {
			case "kill":
				n.Kill()
			case "killprocess":
				n.KillProcess()
			case "stop":
				n.Stop(time.Second)
			}
			<-down
			<-done
			<-n.Down()
			if n.Up() {
				t.Fatalf("%s: node up after Down closed", end)
			}
			fmt.Println(end, "down")
		}
	})
}

func TestStartReturns(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("n", "10.0.0.1")
		served := make(chan bool)
		<-n.Start(func() {
			go func() { served <- true }() // keeps running after the start function returns
		})
		if !n.Up() {
			t.Fatal("node down after its start function returned")
		}
		select {
		case <-n.Down():
			t.Fatal("Down closed after the start function returned")
		default:
		}
		<-served
		n.Kill()
		fmt.Println("start returned")
	})
}
