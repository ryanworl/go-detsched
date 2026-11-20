// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sim

import "strconv"

// A nodeClock is a node's clock: at the simulation's time t, it reads
// base + rate·(t − anchor). The runtime computes the same (detsimClock).
type nodeClock struct {
	anchor, base int64
	rate         float64
}

func (c nodeClock) local(t int64) int64 { return c.base + int64(float64(t-c.anchor)*c.rate) }

// Clock drift: a node draws its clock rate from 1 − maxDrift to
// 1 + maxDrift when setup completes, and a FaultClockRate runs a node's
// clock at a rate from 1/wideRate to wideRate, log-uniformly, until it
// ends.
const (
	maxDrift = 0.01
	wideRate = 4
)

// drawClockRate draws a node's clock rate within maxDrift of 1, in steps
// of ten parts per million.
func drawClockRate() float64 {
	const steps = int(maxDrift * 1e5)
	return 1 + float64(Intn(2*steps+1)-steps)*1e-5
}

// SetClockRate makes the node's clock run at rate r relative to the
// simulation's clock from now on: r 1.01 gains 10ms a second, r 0.5 runs
// at half speed. Everything time-related on the node runs on that clock:
// time.Now, monotonic readings and time.Since, timers, tickers, sleeps,
// context deadlines, socket deadlines, and its kernel's TCP timeouts. The
// clock reads on from where it was, so the node stays consistent with
// itself, and its timers keep their times on the node's clock; it drifts
// against the other nodes, the harness, and the network, whose latencies
// are physical and stay on the simulation's clock. The rate stays across
// restarts, which go on reading the same clock, as a machine's clock runs
// on while its processes restart.
//
// Every harness runs with drift: nodes' clocks run together during setup,
// and when setup completes (see SetCalm), each node whose rate
// SetClockRate did not set draws one within ±1% of 1, as does a node
// added later. Call SetClockRate(1) on a node to keep its clock with the
// simulation's. A Nemesis also runs a node's clock at a rate from 1/4 to
// 4 for the length of a FaultClockRate, then at its rate before again,
// to find timeouts measured on one node and enforced on another.
func (n *Node) SetClockRate(r float64) {
	if !(r > 0) {
		panic("sim: clock rate must be positive")
	}
	n.clockSet = true
	n.setClockRate(r)
}

// drift draws the clock rate of each node whose rate SetClockRate did not
// set, when setup completes, so that runs the search branches after setup
// draw their own. The drift is the fault sim.clock.drift, which the
// detsched command can turn off to find whether a failure needs it.
func (w *World) drift() {
	for _, n := range w.nodes {
		if !n.removed {
			n.drift()
		}
	}
}

func (n *Node) drift() {
	if n.clockSet || runtime_faultOff("", "sim.clock.drift") {
		return
	}
	if r := drawClockRate(); r != 1 {
		n.setClockRate(r)
		injected("fault: sim.clock.drift")
	}
}

func (n *Node) setClockRate(r float64) {
	runtime_call(func() {
		t := now()
		n.clock = nodeClock{anchor: t, base: n.clock.local(t), rate: r}
	})
	n.w.logEvent(n.name, "clock rate", strconv.FormatFloat(r, 'g', 6, 64))
	if n.inc != nil {
		n.applyClock(true)
	}
}

// ClockRate returns the rate of the node's clock relative to the
// simulation's (see SetClockRate).
func (n *Node) ClockRate() float64 { return n.clock.rate }

// applyClock gives the node's current handle its clock. A handle has the
// simulation's clock until then, which costs time.Now nothing.
func (n *Node) applyClock(rescale bool) {
	c := n.clock
	if c.rate == 1 && c.base == c.anchor && !rescale {
		return
	}
	runtime_setClock(n.inc.handle, c.anchor, c.base, c.rate, rescale)
}

// delay returns how long d on the node's clock takes on the simulation's,
// for the timeouts of its kernel.
func (n *Node) delay(d int64) int64 {
	if r := n.clock.rate; r != 1 && r > 0 {
		return max(int64(float64(d)/r+0.5), 1)
	}
	return d
}
