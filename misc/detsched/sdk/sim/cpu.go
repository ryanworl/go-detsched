// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"math"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

// Busy time. A World makes its nodes' CPU work take simulated time: each
// node has a processor with a number of cores and a speed, and a node's
// goroutine that has computed for about a millisecond of the reference
// core sleeps on one of its node's cores for as long as the work would
// take there, so that time passes while a node computes, and a loop that
// never blocks, such as a retry without a backoff, cannot stop the clock.
// Each system call the simulation emulates costs CPU time too:
// syscallCostNs, plus syscallBytePs per byte it copies. The runtime's own
// work, the simulation's, and the work of goroutines of no node, such as
// a test's and the nemesis's, are free.

const (
	syscallCostNs = 1000 // CPU time of a system call
	syscallBytePs = 100  // and of each byte it copies (10 GB/s)

	// longBusy is how long a node's goroutine must compute without
	// waiting for anything before reportBusy notes that busy time let
	// time pass where the clock would have stopped.
	longBusy = 100 * time.Millisecond
)

// UnlimitedCores, as a number of cores, gives every goroutine of a node
// its own core.
const UnlimitedCores = -1

// drawCPU draws the cores and speed of a new node's processor from the
// fault stream: from 1 to 16 cores, or now and then unlimited ones, at a
// speed from 0.5 to 2 times the reference core.
func drawCPU() (cores int, speed float64) {
	cores = UnlimitedCores
	if Intn(8) != 0 {
		cores = 1 + Intn(16)
	}
	u := float64(Rand()&(1<<53-1)) / (1 << 53)
	return cores, 0.5 * math.Exp(u*math.Log(4)) // log-uniform
}

// SetCPU pins the node's processor: its number of cores, or
// UnlimitedCores, and its speed, as a multiple of the reference core,
// such as 0.5 for one that takes twice as long. A zero argument leaves
// that part as it is. The change applies at once if the node is up.
func (n *Node) SetCPU(cores int, speed float64) {
	if cores != 0 {
		n.cores = cores
	}
	if speed > 0 {
		n.speed = speed
	}
	if n.inc != nil {
		n.applyCPU()
	}
}

func (n *Node) applyCPU() {
	runtime_setCPU(n.inc.handle, int32(max(n.cores, 0)), int32(max(math.Round(n.speed*1000), 1)))
}

// CPUStats describes the busy time of a World's nodes so far.
type CPUStats struct {
	Ticks   uint64        // units of work node goroutines did
	Pauses  uint64        // times a node goroutine slept to pay for its work
	Busy    time.Duration // simulated CPU time paid
	Longest time.Duration // the longest one goroutine computed without waiting for anything else
}

// BusyStats returns the busy time of the nodes so far.
func BusyStats() CPUStats {
	ticks, parks, paid, streak, _ := runtime_busyStats()
	return CPUStats{ticks, parks, time.Duration(paid), time.Duration(streak)}
}

// reportBusy reports, when the World closes, whether a node's goroutine
// computed for longer than longBusy without waiting for anything: without
// busy time the clock would have stopped while it ran.
func (w *World) reportBusy() {
	_, _, _, streak, node := runtime_busyStats()
	if time.Duration(streak) >= longBusy {
		name := ""
		if inc := w.incs[node]; inc != nil {
			name = inc.node.name
		}
		assert.Reachable("sim: a node computed for 100ms without waiting, and busy time let the clock advance", map[string]any{"node": name, "busy": time.Duration(streak).String()})
	}
}
