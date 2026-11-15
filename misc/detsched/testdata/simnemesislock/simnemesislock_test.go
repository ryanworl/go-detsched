// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simnemesislock has the nemesis's Event callback take a mutex
// that a goroutine of node a holds, a harness bug: once the nemesis pauses
// a while its goroutine holds the mutex, the callback waits for a node
// that only the nemesis, whose goroutine runs the callback, resumes, and
// node b's ticker moves the clock on forever. The runtime fails the
// program at once instead. With SIMNEMESISLOCK=chan, the callback waits
// for a's goroutine on a channel instead, which the runtime reports once
// the callback has been blocked for a minute of the bubble's time. With
// SIMNEMESISLOCK=normal, the nemesis runs every kind of fault on a, and
// the callbacks wait only for a mutex that node c holds while the test,
// not the nemesis, pauses c, and the program passes; the callbacks check
// that Nemesis.CalmSince turns true at each calm and false at a storm.
package simnemesislock

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestNemesisLock(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	mode := os.Getenv("SIMNEMESISLOCK")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		c := w.AddNode("c", "10.0.0.3")
		b.Start(func() {
			for range time.Tick(100 * time.Millisecond) {
			}
		})

		var shared sync.Mutex // the bug: a's goroutine and the callback take it
		ready := make(chan bool)
		var cmu sync.Mutex // c holds it while the test pauses c
		serve := func() {
			var own sync.Mutex
			for {
				switch mode {
				case "chan":
					select {
					case ready <- true:
					default:
					}
				case "normal":
					own.Lock()
				default:
					shared.Lock()
				}
				time.Sleep(10 * time.Millisecond)
				switch mode {
				case "chan":
				case "normal":
					own.Unlock()
				default:
					shared.Unlock()
				}
				time.Sleep(time.Millisecond)
			}
		}
		a.Start(serve)

		events := 0
		var nm *sim.Nemesis
		cfg := sim.NemesisConfig{
			Nodes: []*sim.Node{a},
			Event: func(ev sim.Event) {
				switch mode {
				case "chan":
					<-ready
				case "normal":
					if since, calm := nm.CalmSince(); calm != (ev.Kind == sim.PhaseCalm) || since.After(time.Now()) {
						t.Errorf("at %q: CalmSince %v, %v", ev.Desc, since, calm)
					}
					cmu.Lock()
					cmu.Unlock()
				default:
					shared.Lock()
					shared.Unlock()
				}
				events++
			},
		}
		if mode != "normal" {
			// Only pauses, as there is no Restart.
			cfg.Disable = []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultThrottle, sim.FaultClockSkip, sim.FaultDisk}
		} else {
			cfg.Restart = func(n *sim.Node) { n.Start(serve) }
			// The callbacks wait for c until the test resumes it,
			// which is no bug, as the nemesis did not pause it.
			c.Start(func() {
				cmu.Lock()
				time.Sleep(30 * time.Second)
				cmu.Unlock()
			})
			time.Sleep(time.Second)
			c.Pause()
		}
		nm = w.NewNemesis(cfg)
		nm.Start()
		if mode == "normal" {
			time.Sleep(40 * time.Second)
			c.Resume()
		}
		time.Sleep(2 * time.Minute)
		nm.Stop()
		if _, calm := nm.CalmSince(); !calm {
			t.Errorf("CalmSince after Stop: false")
		}
		fmt.Println("events:", events, "a up:", a.Up())
	})
}
