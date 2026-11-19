// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// Runaway runs: limits the detsched command sets from the runs that
// passed, so that a run that would not finish ends at a defined switch
// that its replay line stops at too.
//
// GODETSEARCH's extent field turns this on: the run prints how far it
// went at exit (detsched: extent ...), and the switches it made last, by
// goroutine, when it stops, at a limit or at a signal such as the
// command's SIGQUIT. The limits are:
//
//	maxswitches=N   N switches since the last branch (see detMaxSwitches)
//	maxstuck=N      N switches, not counting preemptions, while every
//	                bubble's clock stays put: a livelock, such as goroutines
//	                that wake each other forever, and not a long computation
//	maxtime=NS      a bubble's clock passes NS nanoseconds since its start
//	maxheap=B       a GC finds more than B bytes live
//	stopat=N        switch N, for the replay of a run stopped from outside
//
// Each counts only what does not depend on how the program was compiled
// or on real time, so a run and its replay stop at the same switch.

// detRunaway is the state of the limits. Only code running on the single
// P reads or writes it, except detRunawayHeap, which runs with the world
// stopped.
var detRunaway struct {
	on                             bool
	maxStuck, maxTime, maxHeap, at uint64
	stuckRun                       uint64 // switches but preemptions since a bubble's clock last moved, or a bubble started
	stuck                          uint64 // the most of them before the clock moved
	preempted                      bool   // the next switch is a preemption
	time                           int64  // the furthest a bubble's clock went since its start
	heap                           uint64 // the most bytes a GC found live
	overHeap                       uint64 // what a GC found live over maxHeap, or 0
	stopped                        bool
	next                           guintptr      // the goroutine about to run at the last switch
	ring                           [1024]uint64  // the goroutines of the last switches
	starts                         [1024]uintptr // the functions they started in
}

// detRunawayStuck returns the switches but preemptions since a bubble's
// clock last moved, or 0 when no bubble runs.
func detRunawayStuck() uint64 {
	if detBubbles == nil {
		return 0
	}
	return detRunaway.stuckRun
}

// detRunawayExecute is called by detExecute, after the switch to gp
// counted, when the limits are on.
func detRunawayExecute(pp *p, gp *g) {
	r := &detRunaway
	n := detP(pp).switches
	r.ring[n%uint64(len(r.ring))] = gp.goid
	r.starts[n%uint64(len(r.ring))] = gp.startpc
	r.next.set(gp)
	if r.preempted {
		r.preempted = false
	} else if detBubbles != nil {
		r.stuckRun++
	}
	switch {
	case r.stopped:
	case detMaxSwitches != 0 && n-detBranchSwitch > detMaxSwitches:
		detRunawayStop(n, "switches", n-detBranchSwitch, "switches since the last branch (maxswitches=", detMaxSwitches)
	case r.maxStuck != 0 && detRunawayStuck() > r.maxStuck:
		detRunawayStop(n, "livelock", detRunawayStuck(), "switches, not counting preemptions, while virtual time stayed put (maxstuck=", r.maxStuck)
	case r.maxTime != 0 && uint64(r.time) > r.maxTime:
		detRunawayStop(n, "time", uint64(r.time), "ns of virtual time (maxtime=", r.maxTime)
	case r.overHeap != 0:
		detRunawayStop(n, "memory", r.overHeap, "bytes live after a GC (maxheap=", r.maxHeap)
	case r.at != 0 && n >= r.at:
		detRunawayStop(n, "stop", n, "switches (stopat=", r.at)
	}
}

// detRunawayStop ends the run at switch n for passing a limit, after a
// line that says which. detPanic then prints the last switches.
func detRunawayStop(n uint64, kind string, v uint64, what string, limit uint64) {
	detRunaway.stopped = true
	print("detsched: did not finish: ", kind, ": ", v, " ", what, limit, ") at switch ", n, ", virtual time ")
	detPrintSeconds(detRunaway.time) // see detstop.go
	print("s\n")
	throw("detsched: did not finish")
}

// detRunawayTime is called by detTraceTime when bubble's clock moved.
func detRunawayTime(bubble *synctestBubble) {
	r := &detRunaway
	r.stuck = max(r.stuck, detRunawayStuck())
	r.stuckRun = 0
	r.time = max(r.time, bubble.now-946684800000000000) // since synctest's base time
}

// detRunawayBubble is called when a bubble starts or ends.
func detRunawayBubble(start bool) {
	if !detRunaway.on {
		return
	}
	r := &detRunaway
	if start && detBubbles == nil {
		r.stuckRun = 0
	} else if !start {
		r.stuck = max(r.stuck, detRunawayStuck())
	}
}

// detRunawayHeap is called at the end of each GC's mark phase with the
// bytes it found live. The world is stopped.
func detRunawayHeap(live uint64) {
	r := &detRunaway
	r.heap = max(r.heap, live)
	if r.maxHeap != 0 && live > r.maxHeap && r.overHeap == 0 {
		r.overHeap = live
	}
}

// detRunawayExtent prints how far the run went, which the detsched
// command sets the next runs' limits from.
func detRunawayExtent() {
	if !detRunaway.on {
		return
	}
	r := &detRunaway
	print("detsched: extent time=", r.time, " stuck=", max(r.stuck, detRunawayStuck()), " heap=", r.heap, "\n")
}

// detRunawayReport prints the goroutines the last switches went to, most
// first, each with the functions outside the runtime and the standard
// library's synchronization that it is in now, innermost first, when the
// run stopped at a limit or a signal. Switches to goroutines that have
// exited since count on one line, with the functions they started in.
func detRunawayReport() {
	r := &detRunaway
	if !r.on || !r.stopped && getg().m.caughtsig.ptr() == nil {
		return
	}
	var n uint64
	for _, pp := range allp {
		n += detP(pp).switches
	}
	k := min(n, uint64(len(r.ring)))
	var goids [16]uint64
	var counts [16]uint64
	var other, exited uint64
	var exitedStarts [2]uintptr
	var lastID uint64
	lastLive := false
	for i := uint64(0); i < k; i++ {
		id := r.ring[(n-i)%uint64(len(r.ring))]
		if id != lastID {
			lastID, lastLive = id, detRunawayG(id) != nil
		}
		if !lastLive {
			exited++
			pc := r.starts[(n-i)%uint64(len(r.ring))]
			for j := range exitedStarts {
				if exitedStarts[j] == pc || exitedStarts[j] == 0 {
					exitedStarts[j] = pc
					break
				}
			}
			continue
		}
		j := 0
		for j < len(goids) && goids[j] != id && counts[j] != 0 {
			j++
		}
		if j == len(goids) {
			other++
			continue
		}
		goids[j] = id
		counts[j]++
	}
	// Most switches first, then the lower goroutine ID.
	for i := 1; i < len(goids); i++ {
		for j := i; j > 0 && (counts[j] > counts[j-1] || counts[j] == counts[j-1] && counts[j] != 0 && goids[j] < goids[j-1]); j-- {
			goids[j], goids[j-1] = goids[j-1], goids[j]
			counts[j], counts[j-1] = counts[j-1], counts[j]
		}
	}
	print("detsched: the last ", k, " switches went to:\n")
	for i, c := range counts {
		if c == 0 || i >= 10 {
			other += c
			continue
		}
		print("detsched: \t", c, " goroutine ", goids[i], " ")
		detRunawayWhere(goids[i])
		print("\n")
	}
	if exited > 0 {
		print("detsched: \t", exited, " exited goroutines, started in")
		for j, pc := range exitedStarts {
			if f := findfunc(pc); pc != 0 && f.valid() {
				if j > 0 {
					print(",")
				}
				print(" ", funcname(f))
			}
		}
		print("\n")
	}
	if other > 0 {
		print("detsched: \t", other, " other goroutines\n")
	}
}

// detRunawayG returns the goroutine with ID goid, or nil if it exited.
func detRunawayG(goid uint64) *g {
	var gp *g
	forEachGRace(func(g1 *g) {
		if g1.goid == goid && readgstatus(g1)&^_Gscan != _Gdead {
			gp = g1
		}
	})
	return gp
}

// detRunawayWhere prints where goroutine goid is: the functions outside
// the runtime and the standard library's synchronization it is in, with
// their files and lines, innermost first and separated by " < ", or the
// function it started in.
func detRunawayWhere(goid uint64) {
	gp := detRunawayG(goid)
	if gp == nil {
		print("(exited)")
		return
	}
	// The goroutine a limit stopped the run before is running, but has
	// not run yet.
	if s := readgstatus(gp) &^ _Gscan; s == _Grunnable || s == _Gwaiting || s == _Gsyscall || detRunaway.stopped && gp == detRunaway.next.ptr() {
		frames := 0
		var u unwinder
		for u.init(gp, unwindSilentErrors); u.valid() && frames < 32; u.next() {
			for iu, uf := newInlineUnwinder(u.frame.fn, u.symPC()); uf.valid() && frames < 32; uf = iu.next(uf) {
				sf := iu.srcFunc(uf)
				if name := sf.name(); !detRunawayInternal(name) {
					if frames > 0 {
						print(" < ")
					}
					file, line := iu.fileLine(uf)
					print(name, " ", file, ":", line)
					frames++
				}
			}
		}
		if frames > 0 {
			return
		}
	}
	if f := findfunc(gp.startpc); f.valid() {
		print(funcname(f), " (where it started)")
	}
}

// detRunawayInternal reports whether function name is part of the
// runtime or the standard library's synchronization and timers, which
// detRunawayWhere looks past.
func detRunawayInternal(name string) bool {
	for _, p := range [...]string{"runtime.", "internal/", "sync.", "sync/atomic.", "time.", "testing/synctest."} {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	return false
}
