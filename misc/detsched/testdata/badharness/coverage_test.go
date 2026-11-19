// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package badharness holds harnesses with mistakes that make a search
// look clean while it tests little, or that hide a failure. The summary
// must name each mistake (TestSearchBadHarness).
package badharness

import (
	"bufio"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// TestCoverage passes on every seed, and checks little: its workload ends
// a few milliseconds after the nemesis starts, so that most runs end
// before any fault; the nemesis can fault only one node and restart none;
// a check is never reached; and a state it means to reach never comes.
func TestCoverage(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			l, err := net.Listen("tcp", ":7")
			if err != nil {
				return
			}
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					r := bufio.NewReader(c)
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						c.Write([]byte(line))
					}
				}()
			}
		})
		b.Start(func() {})
		nm := w.NewNemesis(sim.NemesisConfig{Nodes: []*sim.Node{a}})
		nm.Start()
		done := make(chan int)
		b.Go(func() {
			// Nothing listens on port 8, so the first dial fails and the
			// retry path runs in every run.
			got, addr := 0, "10.0.0.1:8"
			var c net.Conn
			for got < 3 {
				var err error
				if c == nil {
					c, err = net.Dial("tcp", addr)
					addr = "10.0.0.1:7"
				}
				if err == nil {
					c.SetDeadline(time.Now().Add(time.Second))
					if _, err = c.Write([]byte("x\n")); err == nil {
						_, err = bufio.NewReader(c).ReadString('\n')
					}
				}
				if err != nil {
					assert.Reachable("a request was retried", nil)
					assert.Always(got < 3, "a retry comes before the last response", nil)
					if c != nil {
						c.Close()
						c = nil
					}
					time.Sleep(10 * time.Millisecond)
					continue
				}
				got++
			}
			if got > 3 {
				assert.Always(c != nil, "a client past 3 responses keeps its connection", nil)
			}
			done <- got
		})
		got := <-done
		assert.Sometimes(got > 3, "a client saw more than 3 responses", nil)
		nm.Stop()
	})
}
