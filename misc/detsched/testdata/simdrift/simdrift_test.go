// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simdrift runs nodes whose clocks run at other rates than the
// simulation's (Node.SetClockRate). TestClock checks that a node stays
// consistent with itself: its sleeps, timers, tickers, context and
// socket deadlines, and time.Since all run on its clock, across a change
// of rate and a restart. The lease tests run a lease that a server
// measures on its clock and a holder on its own: it holds with clocks
// that run together and breaks when the holder's runs slow. TestDrawn,
// run with SIMDRIFT_DRAWN=1, keeps the rates nodes draw when setup
// completes, which break it in some runs. TestSimPrograms and
// TestSearchDrift run them.
package simdrift

import (
	"context"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestClock(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		a.SetClockRate(0.5)
		b.SetClockRate(1)
		if r := a.ClockRate(); r != 0.5 {
			t.Errorf("ClockRate() = %v, want 0.5", r)
		}
		// on runs f on node a and returns the simulated time it took.
		on := func(f func()) time.Duration {
			start := w.Elapsed()
			<-a.Start(f)
			a.Stop(0)
			return w.Elapsed() - start
		}
		elapsed := func(what string, want time.Duration, f func()) {
			t.Helper()
			var local time.Duration
			global := on(func() {
				start := time.Now()
				f()
				local = time.Since(start)
			})
			if local != want || global != 2*want {
				t.Errorf("%s: took %v on the node and %v in the simulation, want %v and %v", what, local, global, want, 2*want)
			}
		}
		elapsed("sleep", 10*time.Second, func() { time.Sleep(10 * time.Second) })
		elapsed("timer", 3*time.Second, func() { <-time.NewTimer(3 * time.Second).C })
		elapsed("after func", 3*time.Second, func() {
			done := make(chan bool)
			time.AfterFunc(3*time.Second, func() { close(done) })
			<-done
		})
		elapsed("ticker", 3*time.Second, func() {
			tk := time.NewTicker(time.Second)
			for range 3 {
				<-tk.C
			}
			tk.Stop()
		})
		// At rate 0.5, the node's clock reads the same at two
		// successive simulated nanoseconds. Woken between them, by the
		// harness, the node resets a timer to fire now, which must
		// fire now and not before.
		wake := make(chan bool)
		reset := a.Start(func() {
			fired := make(chan bool)
			tm := time.AfterFunc(time.Hour, func() { fired <- true })
			for range 2 {
				<-wake
				tm.Reset(0)
				<-fired
			}
		})
		for range 2 {
			time.Sleep(1)
			wake <- true
		}
		<-reset
		a.Stop(0)
		elapsed("context", 5*time.Second, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			<-ctx.Done()
			if d, _ := ctx.Deadline(); time.Now().Before(d) {
				t.Errorf("context done at %v, before its deadline %v", time.Now(), d)
			}
		})

		// A socket deadline on a's clock, reading from b, which sends
		// nothing.
		<-b.Start(func() {
			ln, err := net.Listen("tcp", ":7000")
			if err != nil {
				t.Error(err)
				return
			}
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					defer c.Close()
				}
			}()
		})
		on(func() {
			c, err := net.Dial("tcp", "10.0.0.2:7000")
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			start, global := time.Now(), w.Elapsed()
			c.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("read: %v, want a deadline error", err)
			}
			// The node's clock reads whole nanoseconds, which at rate 0.5
			// a simulated nanosecond may not change.
			if local, global := time.Since(start), w.Elapsed()-global; local != 2*time.Second || global < 4*time.Second-1 || global > 4*time.Second {
				t.Errorf("read deadline: took %v on the node and %v in the simulation, want 2s and 4s (less 1ns)", local, global)
			}
		})

		// A change of rate during a sleep: the rest of it runs at the
		// new rate. 4s of the sleep pass at rate 0.5, in 8s; the other
		// 6s at rate 2, in 3s.
		var local time.Duration
		start := w.Elapsed()
		done := a.Start(func() {
			s := time.Now()
			time.Sleep(10 * time.Second)
			local = time.Since(s)
		})
		time.Sleep(8 * time.Second)
		a.SetClockRate(2)
		<-done
		a.Stop(0)
		if global := w.Elapsed() - start; local != 10*time.Second || global != 11*time.Second {
			t.Errorf("sleep across a change of rate: took %v on the node and %v in the simulation, want 10s and 11s", local, global)
		}

		// A restart keeps the rate and reads on from the same clock.
		var before, after time.Time
		<-a.Start(func() { before = time.Now() })
		a.Kill()
		time.Sleep(time.Second)
		<-a.Start(func() { after = time.Now() })
		if d := after.Sub(before); a.ClockRate() != 2 || d != 2*time.Second {
			t.Errorf("after a restart, rate %v and the node's clock moved %v in a second, want 2 and 2s", a.ClockRate(), d)
		}
		a.Stop(0)
		b.Stop(0)
	})
}

// lease runs a server on s, which grants a lease for leaseTime measured
// on its clock, and two clients on a and b that take it in turn. A client
// that gets it holds it for leaseTime less a margin, measured on its own
// clock from when it asked. It returns the simulated time during which
// both clients held the lease at once.
func lease(t *testing.T, w *sim.World, s, a, b *sim.Node) time.Duration {
	const leaseTime, margin = 10 * time.Second, 100 * time.Millisecond
	type req struct {
		reply chan bool
	}
	reqs := make(chan req)
	var expiry time.Time
	s.Start(func() {
		for r := range reqs {
			ok := !time.Now().Before(expiry)
			if ok {
				expiry = time.Now().Add(leaseTime)
			}
			r.reply <- ok
		}
	})
	var holding [2]atomic.Bool
	stop := make(chan bool)
	for i, n := range []*sim.Node{a, b} {
		n.Start(func() {
			for {
				asked := time.Now()
				r := req{make(chan bool)}
				select {
				case reqs <- r:
				case <-stop:
					return
				}
				if <-r.reply {
					holding[i].Store(true)
					time.Sleep(time.Until(asked.Add(leaseTime - margin)))
					holding[i].Store(false)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
	var both time.Duration
	for range 6000 { // a minute
		time.Sleep(10 * time.Millisecond)
		if holding[0].Load() && holding[1].Load() {
			both += 10 * time.Millisecond
		}
	}
	close(stop)
	return both
}

// TestLeaseTogether runs the lease with clocks that run together: no
// two clients hold it at once.
func TestLeaseTogether(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		s, a, b := w.AddNode("s", "10.0.0.1"), w.AddNode("a", "10.0.0.2"), w.AddNode("b", "10.0.0.3")
		for _, n := range []*sim.Node{s, a, b} {
			n.SetClockRate(1)
		}
		if both := lease(t, w, s, a, b); both != 0 {
			t.Errorf("with clocks that run together, two clients held the lease for %v", both)
		}
	})
}

// TestLeaseSlow runs the lease with a holder's clock 2% slow, more than
// its margin allows: it holds on after the server granted the lease to
// the other client.
func TestLeaseSlow(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		s, a, b := w.AddNode("s", "10.0.0.1"), w.AddNode("a", "10.0.0.2"), w.AddNode("b", "10.0.0.3")
		s.SetClockRate(1)
		a.SetClockRate(0.98)
		b.SetClockRate(0.98)
		if both := lease(t, w, s, a, b); both == 0 {
			t.Errorf("with the holders' clocks 2%% slow, no two clients held the lease at once")
		}
	})
}

// TestDrawn runs the lease with the clock rates the nodes draw when setup
// completes.
func TestDrawn(t *testing.T) {
	if !sim.Supported() || os.Getenv("SIMDRIFT_DRAWN") == "" {
		t.Skip("needs GOEXPERIMENT=detsched and SIMDRIFT_DRAWN=1")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		s, a, b := w.AddNode("s", "10.0.0.1"), w.AddNode("a", "10.0.0.2"), w.AddNode("b", "10.0.0.3")
		lifecycle.SetupComplete(nil)
		if both := lease(t, w, s, a, b); both != 0 {
			t.Errorf("two clients held the lease for %v; clock rates s %v, a %v, b %v", both, s.ClockRate(), a.ClockRate(), b.ClockRate())
		}
	})
}
