// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Busy time: the CPU work of a simulation's nodes takes simulated time.
//
// A bubble's clock advances only when all its goroutines are blocked, so
// without busy time a goroutine that keeps running never lets time pass:
// a retry loop without a sleep, or a spin on a deadline, spins forever,
// which no real machine does. With busy time, which a simulation turns on
// (detsimBusyOn), the work a node's goroutine does is debt that it pays by
// sleeping, as if its computation had taken that long on the node's CPU:
//
//   - Work is counted in ticks (see detsched.go), which the compiler
//     counts in user code only, so the runtime's own work, such as GC and
//     scheduling, is free, as is the simulation's (detsimEnter). A tick is
//     detBusyPsPerTick picoseconds of a reference core. The simulation
//     adds the CPU cost of each system call it emulates (detsimBusyAdd).
//     Goroutines of no node never owe anything.
//   - Once a goroutine owes detBusyQuantumPs, it stops at its next tick
//     and sleeps on its bubble's clock, on the earliest free of its node's
//     cores (detsimSetCPU), until max(now, when that core is free) + its
//     debt at the node's speed, and the core is busy until then. A node
//     with unlimited cores has one per goroutine. Debt is never forgiven:
//     a goroutine that exits owing less than a quantum leaves its debt to
//     the next goroutine of its node that runs (detBusyExit), so a chain
//     of short goroutines, each starting the next, pays as one would.
//   - The clock still advances only when every goroutine is blocked or
//     sleeping, so other nodes keep running at the same instant while one
//     pays, and their timers fire at their own times.
//
// Everything is counted, never measured, so the schedule stays a function
// of the seed.

// detBusyPsPerTick is the CPU time of one tick on the reference core, so
// that simulated CPU time roughly matches one modern core. Ticks leave out
// the runtime's work, such as channel operations, goroutine switches and
// timers, which this rate covers on average. It was calibrated with
// go1.27.4-detsched on one core of an Intel Xeon @ 2.60GHz, with GC off
// (with a harness that is not published), as the
// geometric mean of two workloads: an LSM key-value store writing 200k
// keys, flushing and compacting on an in-memory file system, 634ms of CPU
// time and 75,496,820 ticks (8,400ps a tick); and a 3-node consensus
// cluster applying 20,000 entries over in-memory transports, 264ms and
// 11,617,195 ticks (22,700ps a tick).
const detBusyPsPerTick = 13800

// detBusyQuantumPs is how much CPU time a goroutine owes before it pays.
const detBusyQuantumPs = 1e9 // 1ms

var detBusy struct {
	on   bool
	last *g    // the goroutine whose ticks run down detTicks
	q    int32 // detTicksReal() when last's ticks were last counted

	cpus []detBusyCPU // by node handle

	// carry is the debt goroutines of each node left when they exited,
	// by node handle (detBusyExit). detsimNewproc grows it to cover the
	// node of every goroutine it starts.
	carry []int64

	ticks  uint64 // ticks node goroutines ran
	parks  uint64 // times a goroutine slept to pay
	paidNs int64  // simulated CPU time paid
	// The longest stretch of CPU time, in ns, a goroutine paid without
	// waiting for anything else in between, and its node.
	streakNs   int64
	streakNode int32
}

// A detBusyCPU is a node's processor.
type detBusyCPU struct {
	speed int64   // permille of the reference core
	free  []int64 // when each core is next free; nil: unlimited cores
}

// detBusyState is a goroutine's busy-time state, part of detGState.
type detBusyState struct {
	goid     uint64 // the goroutine the state belongs to: g structs are reused
	debt     int64  // CPU time owed, in ps of the reference core
	lastWake int64  // when it last woke from paying
	streak   int64  // when its current stretch of paying began
}

// detBusyOf returns gp's busy-time state, reset if gp is a new goroutine.
//
//go:nosplit
func detBusyOf(gp *g) *detBusyState {
	s := &detG(gp).busy
	if s.goid != gp.goid {
		*s = detBusyState{goid: gp.goid}
	}
	return s
}

// detBusyCount charges the ticks run since the last count to the goroutine
// that ran them.
//
//go:nosplit
func detBusyCount() {
	ticks := detTicksReal()
	if gp := detBusy.last; gp != nil && detBusy.q > ticks {
		if detG(gp).node != 0 {
			n := int64(detBusy.q - ticks)
			detBusyOf(gp).debt += n * detBusyPsPerTick
			detBusy.ticks += uint64(n)
		}
	}
	detBusy.q = ticks
}

// detSetTicks sets detTicks, after counting the ticks run so far.
//
//go:nosplit
func detSetTicks(n int32) {
	if detBusy.on {
		detBusyCount()
	}
	detPutTicks(n)
	detBusy.q = n
}

// detBusyClamp makes the running goroutine stop at the tick where its
// debt reaches a quantum. Called when gp starts a slice (detExecute) and
// when its debt grows by other means (detsimExit).
//
//go:nosplit
func detBusyClamp(gp *g) {
	detBusyCount()
	detBusy.last = gp
	if node := detG(gp).node; node != 0 {
		s := detBusyOf(gp)
		if int(node) < len(detBusy.carry) {
			s.debt += detBusy.carry[node]
			detBusy.carry[node] = 0
		}
		left := (detBusyQuantumPs - s.debt + detBusyPsPerTick - 1) / detBusyPsPerTick
		if int64(detTicksReal()) > max(left, 1) {
			detPutTicks(int32(max(left, 1)))
		}
	}
	detBusy.q = detTicksReal()
	if detPCT.last == gp {
		detPCT.lastQ = detBusy.q
	}
}

// detBusyPay makes the running goroutine pay its debt, if it owes a
// quantum, by sleeping until its node's CPU would have done the work. It
// reports whether it did. Called by detPreempt.
func detBusyPay() bool {
	gp := getg()
	detBusyCount()
	s := detBusyOf(gp)
	node := detG(gp).node
	bubble := gp.bubble
	if node == 0 || s.debt < detBusyQuantumPs || bubble == nil || !canPreemptM(gp.m) {
		return false
	}
	t := gp.timer
	if t == nil {
		t = new(timer)
		t.init(goroutineReady, gp)
		t.isFake = true
		gp.timer = t
	}
	if !t.isFake {
		return false // its timer is from before it joined the bubble
	}
	now := bubble.now
	speed, free := int64(1000), []int64(nil)
	if int(node) < len(detBusy.cpus) && detBusy.cpus[node].speed > 0 {
		speed, free = detBusy.cpus[node].speed, detBusy.cpus[node].free
	}
	cost := max(s.debt/speed, 1) // ns
	wake := now + cost
	if free != nil {
		i := 0
		for j := range free {
			if free[j] < free[i] {
				i = j
			}
		}
		wake = max(now, free[i]) + cost
		free[i] = wake
	}
	s.debt = 0
	if s.lastWake != now || s.streak == 0 {
		s.streak = now // it waited for something else since it last paid
	}
	s.lastWake = wake
	if wake-s.streak > detBusy.streakNs {
		detBusy.streakNs, detBusy.streakNode = wake-s.streak, node
	}
	detBusy.parks++
	detBusy.paidNs += cost
	gp.sleepWhen = wake
	resetForSleep(gp, nil)
	gopark(nil, nil, waitReasonSleep, traceBlockSleep, 1)
	return true
}

// detBusyGrow makes detBusy.carry cover node. Called by detsimNewproc.
func detBusyGrow(node int32) {
	for int(node) >= len(detBusy.carry) {
		detBusy.carry = append(detBusy.carry, 0)
	}
}

// detBusyExit leaves the debt of gp, which is exiting, to the next
// goroutine of its node that starts a slice (detBusyClamp). Called by
// gdestroy.
//
//go:nosplit
func detBusyExit(gp *g) {
	if !detBusy.on {
		return
	}
	detBusyCount()
	if detBusy.last == gp {
		detBusy.last = nil
	}
	node := detG(gp).node
	if s := detBusyOf(gp); node != 0 && int(node) < len(detBusy.carry) {
		detBusy.carry[node] += s.debt
		s.debt = 0
	}
}

// detsimEnter keeps the simulation's own work, which runs on the system
// stack, from counting as the running goroutine's ticks or preempting it.
// It returns what detsimExit restores.
//
//go:nosplit
func detsimEnter() (ticks, q int32, last *g) {
	ticks, q, last = detTicksReal(), detBusy.q, detBusy.last
	detPutTicks(1<<31 - 1)
	detBusy.q, detBusy.last = 1<<31-1, nil
	return
}

// detsimExit undoes detsimEnter. A goroutine the simulation's work made
// runnable may have asked the running one to yield (detWake), which
// survives.
//
//go:nosplit
func detsimExit(ticks, q int32, last *g) {
	n := min(ticks, detTicksReal())
	detPutTicks(n)
	detBusy.q, detBusy.last = q-(ticks-n), last
	if detBusy.on && last != nil && last == getg().m.curg {
		detBusyClamp(last)
	}
}

// detsimBusyOn turns busy time on or off.
func detsimBusyOn(on bool) {
	detBusy.on = on
	detBusy.last, detBusy.q = getg().m.curg, detTicksReal()
	if detBusy.on {
		detBusy.ticks, detBusy.parks, detBusy.paidNs, detBusy.streakNs, detBusy.streakNode = 0, 0, 0, 0, 0
	}
}

// detsimSetCPU gives the node with the given handle a processor with the
// given number of cores, or unlimited ones if cores <= 0, each running at
// speed permille of the reference core.
func detsimSetCPU(node int32, cores, speed int32) {
	systemstack(func() {
		for int(node) >= len(detBusy.cpus) {
			detBusy.cpus = append(detBusy.cpus, detBusyCPU{})
		}
		c := detBusyCPU{speed: int64(max(speed, 1))}
		if cores > 0 {
			c.free = make([]int64, cores)
		}
		detBusy.cpus[node] = c
	})
}

// detsimBusyAdd adds ns of reference CPU time to the debt of the running
// goroutine, if it belongs to a node: the cost of a system call the
// simulation emulates. It may run on the system stack.
//
//go:nosplit
func detsimBusyAdd(ns int64) {
	gp := getg().m.curg
	if !detBusy.on || gp == nil || detG(gp).node == 0 {
		return
	}
	detBusyOf(gp).debt += ns * 1000
}

// detsimBusyStats returns the ticks node goroutines ran, how often they
// slept to pay, the CPU time they paid, and the longest stretch one paid
// without waiting for anything else, with its node.
func detsimBusyStats() (ticks, parks uint64, paidNs, streakNs int64, streakNode int32) {
	return detBusy.ticks, detBusy.parks, detBusy.paidNs, detBusy.streakNs, detBusy.streakNode
}

// detBusyReport adds busy time to the dettrace report.
func detBusyReport() {
	if detBusy.ticks > 0 {
		print("detsched: busy ticks=", detBusy.ticks, " parks=", detBusy.parks, " paid=", detBusy.paidNs, "ns longest=", detBusy.streakNs, "ns\n")
	}
}
