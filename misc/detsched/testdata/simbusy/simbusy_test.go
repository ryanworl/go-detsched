// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simbusy checks that the CPU work of a simulation's nodes takes
// simulated time (busy time; see detsched/sdk/sim's cpu.go): a spin on a
// deadline and a retry loop without a backoff end, a node's computing
// delays neither another node's timers nor its work, and a node's cores
// bound how much of its work overlaps. TestSimPrograms runs it.
package simbusy

import (
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var sink uint64

// work computes for about n units: n×100,000 loop iterations.
//
//go:noinline
func work(n int) {
	x := sink
	for i := range n * 100_000 {
		x = x*6364136223846793005 + uint64(i)
	}
	sink = x
}

// run runs f on node n and waits for it.
func run(n *sim.Node, f func()) {
	done := make(chan struct{})
	n.Go(func() { f(); close(done) })
	<-done
}

func TestBusy(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.SetCPU(sim.UnlimitedCores, 1)
		b.SetCPU(sim.UnlimitedCores, 1)
		a.Start(func() {})
		b.Start(func() {})

		// One unit of work, measured.
		var unit time.Duration
		run(a, func() {
			start := time.Now()
			work(200)
			unit = time.Since(start) / 200
		})
		if unit <= 0 {
			t.Fatalf("work took no simulated time")
		}
		t.Logf("one unit of work takes %v", unit)

		// A spin on a deadline ends at the deadline.
		run(a, func() {
			deadline := time.Now().Add(50 * time.Millisecond)
			for time.Now().Before(deadline) {
			}
			if late := time.Since(deadline); late > 2*time.Millisecond {
				t.Errorf("spin on a deadline ended %v late", late)
			}
		})

		// A retry loop without a backoff ends when the fault it retries
		// ends, as a full disk does after a while.
		run(a, func() {
			var mu sync.Mutex
			full := true
			time.AfterFunc(300*time.Millisecond, func() { mu.Lock(); full = false; mu.Unlock() })
			start, tries := time.Now(), 0
			for {
				mu.Lock()
				f := full
				mu.Unlock()
				if !f {
					break
				}
				tries++
			}
			if d := time.Since(start); d < 300*time.Millisecond || d > 302*time.Millisecond {
				t.Errorf("retry loop ran %v, want 300ms", d)
			}
		})

		// A retry loop whose every try is a new goroutine, as a flush
		// that fails and schedules the next one in a goroutine is, ends
		// too: each try owes less than a quantum, which the goroutine
		// leaves to the next one.
		run(a, func() {
			var mu sync.Mutex
			full := true
			time.AfterFunc(300*time.Millisecond, func() { mu.Lock(); full = false; mu.Unlock() })
			start := time.Now()
			done := make(chan struct{})
			var try func()
			try = func() {
				x := sink // a tenth of a unit of work, under a quantum
				for i := range 10_000 {
					x = x*6364136223846793005 + uint64(i)
				}
				sink = x
				mu.Lock()
				f := full
				mu.Unlock()
				if f {
					go try()
				} else {
					close(done)
				}
			}
			go try()
			<-done
			if d := time.Since(start); d < 300*time.Millisecond || d > 302*time.Millisecond {
				t.Errorf("retry chain of goroutines ran %v, want 300ms", d)
			}
		})

		// While a computes for 300ms, b's 100ms timer fires on time.
		start := time.Now()
		var bWoke, aDone time.Duration
		var wg sync.WaitGroup
		wg.Add(2)
		a.Go(func() { work(int(300 * time.Millisecond / unit)); aDone = time.Since(start); wg.Done() })
		b.Go(func() { time.Sleep(100 * time.Millisecond); bWoke = time.Since(start); wg.Done() })
		wg.Wait()
		if bWoke != 100*time.Millisecond {
			t.Errorf("b's 100ms timer fired at %v while a computed", bWoke)
		}
		if aDone < 297*time.Millisecond || aDone > 303*time.Millisecond {
			t.Errorf("a's 300ms of work took %v", aDone)
		}

		// Work on two nodes overlaps; work on one node's cores overlaps as
		// far as it has cores.
		para := func(nodes ...*sim.Node) time.Duration {
			start := time.Now()
			var wg sync.WaitGroup
			for _, n := range nodes {
				wg.Add(1)
				n.Go(func() { work(int(50 * time.Millisecond / unit)); wg.Done() })
			}
			wg.Wait()
			return time.Since(start).Round(5 * time.Millisecond)
		}
		if d := para(a, b); d != 50*time.Millisecond {
			t.Errorf("50ms of work on each of two nodes took %v", d)
		}
		a.SetCPU(1, 0)
		if d := para(a, a); d != 100*time.Millisecond {
			t.Errorf("50ms of work twice on a node with 1 core took %v", d)
		}
		a.SetCPU(2, 0)
		if d := para(a, a, a, a); d != 100*time.Millisecond {
			t.Errorf("50ms of work 4 times on a node with 2 cores took %v", d)
		}
		a.SetCPU(2, 0.5)
		if d := para(a, a); d != 100*time.Millisecond {
			t.Errorf("50ms of work twice on a node with 2 half-speed cores took %v", d)
		}

		// System calls cost CPU time: 1000 writes of 10kB take about
		// 1000 × (1µs + 1µs).
		before := sim.BusyStats().Busy
		run(b, func() {
			f, err := os.Create("/f")
			if err != nil {
				t.Error(err)
				return
			}
			buf := make([]byte, 10000)
			for range 1000 {
				f.Write(buf)
			}
			f.Close()
		})
		if d := sim.BusyStats().Busy - before; d < 2*time.Millisecond {
			t.Errorf("1000 writes of 10kB cost %v of CPU time, want at least 2ms", d)
		}
		st := sim.BusyStats()
		t.Logf("busy: %d ticks, %d pauses, %v paid, longest %v", st.Ticks, st.Pauses, st.Busy, st.Longest)
	})
}
