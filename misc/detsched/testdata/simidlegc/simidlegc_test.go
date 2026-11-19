// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simidlegc checks that a run in which only the simulation's
// events run, here idle connections' keepalives for a week of virtual
// time while every goroutine waits, collects their garbage. The events
// run on the system stack, where an allocation starts no GC, and before,
// nothing else did: the heap grew by gigabytes, without a GC, until the
// machine ran out of memory. TestSimPrograms runs it.
package simidlegc

import (
	"bufio"
	"net"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestIdle(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.Start(func() {})
		b.Start(func() {
			l, err := net.Listen("tcp", ":7")
			if err != nil {
				panic(err)
			}
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go bufio.NewReader(c).ReadString('\n')
			}
		})
		var conns []net.Conn
		done := make(chan bool)
		a.Go(func() {
			for range 16 {
				c, err := net.Dial("tcp", "10.0.0.2:7")
				if err != nil {
					panic(err)
				}
				conns = append(conns, c)
			}
			done <- true
		})
		<-done
		time.Sleep(7 * 24 * time.Hour)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > 256<<20 {
			t.Fatalf("after a week of keepalives, %d MB of heap, after %d GCs", m.HeapAlloc>>20, m.NumGC)
		}
		for _, c := range conns {
			c.Close()
		}
	})
}
