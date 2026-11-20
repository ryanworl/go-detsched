// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/bytealg"
	"internal/stringslite"
	"unsafe"
)

// Stops: points of a run where the runtime stops for a debugger, so that
// a debugger's breakpoints can be set only where they are wanted. Each
// breakpoint hit costs a trap into the debugger, even when its condition
// is false, and a hot function's hits before the point of interest can
// cost minutes; a stop costs one trap, and nothing without a debugger.
//
// GODETSTOP lists the points, separated by commas:
//
//	tD    D seconds of virtual time since the bubble of the goroutine
//	      about to run started (decimal, up to 9 digits after the point),
//	      the seconds of a simulation's fault timeline when the World is
//	      created as the bubble starts
//	sN    switch N, as GODEBUG=dettrace=2 numbers them
//	kN    tick N: after N ticks, counted from the start of the program
//
// each optionally followed by "*M", to stop again every M seconds,
// switches, or ticks after it ("s*M" is "sM*M"), and by ":dump". The run
// stops at the first tick at or after each point: the runtime prints
//
//	detsched: stop I at tD sN kN goroutine G in F (FILE:LINE) trace=0xH
//
// to standard error, with the current position (t- outside a bubble),
// the function and line goroutine G is at, inlined or not, and the hash
// of the schedule so far, which the trace line at exit ends with, prints the stack of every goroutine for ":dump", and calls detStop(I),
// on which a debugger breaks: in a debugger, the frame below detPreempt
// is where goroutine G is. When the stop is at the last tick of G's
// quantum, detPreempt yields after it, as it would have without the
// stop, so G's code goes on only at a later switch; detDebug.yields says
// so. Then the run goes on, as if it had not
// stopped: the stops draw no randomness, run no code that counts ticks,
// and leave every goroutine's budget of ticks as it was.
//
// So without a debugger, stops cost little and change nothing, and their
// lines locate where two runs that should be the same part: run a seed
// on two machines, or two builds, with GODETSTOP=s*100000, and the first
// stop line that differs brackets the switch where they do; then narrow
// it with a finer period from the last line that agrees, such as
// GODETSTOP=s1200000*1000, down to the switch, and to the tick with kN*M,
// whose lines differ in the goroutine's position where the runs part
// within one goroutine's quantum.
//
// Ticks are the run's finest clock: the compiler counts one at every
// function entry and loop back edge in user code (see detsched.go), so no
// two hits of a breakpoint in user code have the same tick, and
// detTickEnd-int64(detTicks) is the tick now. A debugger that records it
// at a breakpoint can replay to just before that hit with GODETSTOP=kN.
//
// The Delve that the release bundles drives the same machinery through
// detDebug instead (see misc/release/delve): it writes a point there
// before the program starts, to run a replay again to a position, and the
// run calls detDebugStop at it, without a stop line.

// detStopNone is detStops.next when no stop is due.
const detStopNone = 1 << 62

// detBubbleStart is the time at which every synctest bubble starts,
// midnight UTC 2000-01-01.
const detBubbleStart = 946684800000000000

// A detStopPoint is a point of GODETSTOP.
type detStopPoint struct {
	kind  byte   // 't', 's', or 'k'
	n     uint64 // nanoseconds, switch, or tick
	every uint64 // stop again this many nanoseconds, switches, or ticks later; 0 is never
	dump  bool   // print every goroutine's stack
	armed bool   // reached: it stops at the next tick
	fired bool
}

var detStops = struct {
	points []detStopPoint
	next   int64 // the tick at which the next stop is due
	on     bool  // there are points, of GODETSTOP or detDebug.stop
}{next: detStopNone}

// detDebug is written by a debugger while the program is stopped, and
// read by the runtime: the bundled Delve's side of stopping a run at a
// position and of counting the entries of a function. Like
// detsimWatched, it holds no pointers, so that adding it changed no
// GC's timing.
var detDebug struct {
	// stop is a point, as of GODETSTOP, at which the run calls
	// detDebugStop; kind 0 is none. The debugger writes it before the
	// program starts.
	stop detStopPoint

	// yields is set by the runtime at a stop, of GODETSTOP or of stop:
	// whether the goroutine yields there, at the last tick of its
	// quantum, so that its code goes on only after other goroutines
	// ran. A debugger that leaves the runtime for the goroutine's code
	// would reach it at a later switch, so it stays at the stop, where
	// the frame below detPreempt is the goroutine's code.
	yields bool

	// watch counts the entries of the functions whose entry tick the
	// debugger made call detPreempt every time; see detWatchEntry. The
	// slots up to nwatch are in use.
	nwatch int32
	watch  [16]detWatch
}

// A detWatch counts the entries of a function.
type detWatch struct {
	pc    uintptr // the return address of the entry tick's call to detPreempt
	count uint64  // the entries counted
	at    uint64  // calls detDebugWatch when count reaches it; 0 is never
}

// detWatchEntry counts an entry for the watch whose pc is the return
// address of a call to detPreempt, and reports whether there is one. When
// there is and the goroutine has ticks left, the call is one that the
// debugger made the entry tick take, and detPreempt returns at once, as
// the tick would have without the debugger: counting changes nothing in
// the run.
//
//go:nosplit
func detWatchEntry(pc uintptr) bool {
	for i := range detDebug.watch[:detDebug.nwatch] {
		w := &detDebug.watch[i]
		if w.pc == pc {
			w.count++
			if w.count == w.at {
				detDebugWatch(i)
			}
			return true
		}
	}
	return false
}

// detDebugWatch is where a debugger breaks when watch i reaches its count.
//
//go:noinline
//go:nosplit
func detDebugWatch(i int) {}

// detDebugStop is where a debugger breaks at detDebug.stop.
//
//go:noinline
//go:nosplit
func detDebugStop() {}

// detStopAt returns point i of GODETSTOP, or detDebug.stop for -1.
//
//go:nosplit
func detStopAt(i int) *detStopPoint {
	if i < 0 {
		return &detDebug.stop
	}
	return &detStops.points[i]
}

// The tick clock. detTickEnd is the tick at which detTicks reaches zero.
// detTickExtra is the running goroutine's ticks that detPutTicks held back
// so that detTicks reaches zero at detStops.next, and gives back once the
// stop is done.
var (
	detTickEnd   int64
	detTickExtra int32
)

// detTickNow returns the number of ticks counted since the program
// started.
//
//go:nosplit
func detTickNow() int64 { return detTickEnd - int64(detTicks) }

// detTicksReal returns the running goroutine's budget of ticks, including
// those held back for a stop.
//
//go:nosplit
func detTicksReal() int32 { return detTicks + detTickExtra }

// detPutTicks sets the running goroutine's budget to n ticks, holding back
// the ticks after the next stop.
//
//go:nosplit
func detPutTicks(n int32) {
	now := detTickNow()
	extra := int32(0)
	if detStops.next != detStopNone {
		if d := max(detStops.next-now, 1); int64(n) > d {
			extra, n = n-int32(d), int32(d)
		}
	}
	detTicks, detTickExtra, detTickEnd = n, extra, now+int64(n)
}

// detStopInit parses GODETSTOP. Called by detRandInit.
func detStopInit() {
	detStops.on = detDebug.stop.kind != 0
	s := gogetenv("GODETSTOP")
	if s == "" {
		if detStops.on {
			detStopsNext()
		}
		return
	}
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			n++
		}
	}
	// Outside the heap, which must not depend on the stops.
	points := unsafe.Slice((*detStopPoint)(persistentalloc(uintptr(n)*unsafe.Sizeof(detStopPoint{}), 8, &memstats.other_sys)), n)[:0]
	for s != "" {
		item := s
		if i := bytealg.IndexByteString(s, ','); i >= 0 {
			item, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		var p detStopPoint
		if len(item) > 5 && item[len(item)-5:] == ":dump" {
			p.dump, item = true, item[:len(item)-5]
		}
		ok := len(item) > 1
		if ok {
			p.kind = item[0]
			at, every, periodic := stringslite.Cut(item[1:], "*")
			if periodic {
				p.every, ok = detParseStop(p.kind, every)
				ok = ok && p.every > 0
				p.n = p.every
			}
			if ok && (at != "" || !periodic) {
				p.n, ok = detParseStop(p.kind, at)
			}
		}
		if !ok {
			print("detsched: GODETSTOP point ", item, "\n")
			fatal("GODETSTOP must be a list of points tD, sN, or kN, each optionally followed by *M and by :dump")
		}
		points = append(points, p)
	}
	detStops.points = points
	detStops.on = true
	detStopsNext()
}

// detParseStop parses the number of a GODETSTOP point of kind kind.
func detParseStop(kind byte, v string) (uint64, bool) {
	switch kind {
	case 't':
		return detParseSeconds(v)
	case 's', 'k':
		n, ok := atoi64u(v)
		return n, ok && n < detStopNone
	}
	return 0, false
}

// detParseSeconds parses decimal seconds, with up to 9 digits after the
// point, into nanoseconds.
func detParseSeconds(s string) (uint64, bool) {
	whole, frac, _ := stringslite.Cut(s, ".")
	if whole == "" && frac == "" || len(frac) > 9 {
		return 0, false
	}
	var w uint64
	if whole != "" {
		var ok bool
		if w, ok = atoi64u(whole); !ok || w > 1<<32 {
			return 0, false
		}
	}
	var f uint64
	for i := 0; i < 9; i++ {
		f *= 10
		if i < len(frac) {
			d := uint64(frac[i] - '0')
			if d > 9 {
				return 0, false
			}
			f += d
		}
	}
	return w*1e9 + f, true
}

// detStopsCheck arms the switch and time points that gp's switch reaches,
// and makes gp's budget run out at the next stop. Called by detExecute.
func detStopsCheck(pp *p, gp *g) {
	armed := false
	for i := -1; i < len(detStops.points); i++ {
		p := detStopAt(i)
		if p.kind == 0 || p.fired || p.armed {
			continue
		}
		switch p.kind {
		case 's':
			p.armed = detP(pp).switches >= p.n
		case 't':
			p.armed = gp.bubble != nil && uint64(gp.bubble.now-detBubbleStart) >= p.n
		}
		armed = armed || p.armed
	}
	if armed {
		detStopsNext()
	}
	detPutTicks(detTicksReal())
}

// detStopsNext sets detStops.next to the tick at which the next stop is
// due: the next tick for an armed point, else the earliest tick point not
// yet fired.
//
//go:nosplit
func detStopsNext() {
	next := detGC.next // a GC epoch is due as a stop is
	for i := -1; i < len(detStops.points); i++ {
		p := detStopAt(i)
		switch {
		case p.kind == 0, p.fired:
		case p.armed:
			next = min(next, detTickNow()+1)
		case p.kind == 'k':
			next = min(next, int64(p.n))
		}
	}
	detStops.next = next
}

// detStopFire stops at the stops due at the current tick, in goroutine gp,
// whose frame below detPreempt is at pc and sp, and gives gp back the
// ticks held back for them. Called by detPreempt. It runs on gp's stack
// only what cannot grow it, as a stack that grows only when stopping
// would take memory from the heap.
//
//go:nosplit
func detStopFire(gp *g, pc, sp uintptr) {
	now := detTickNow()
	// detPreempt yields after the stops, as it would have without them,
	// unless they give back ticks or gp cannot be preempted here.
	detDebug.yields = detTicksReal() <= 0 && canPreemptM(gp.m)
	for i := -1; i < len(detStops.points); i++ {
		p := detStopAt(i)
		if p.kind == 0 || p.fired || !p.armed && !(p.kind == 'k' && int64(p.n) <= now) {
			continue
		}
		p.fired = true
		if i < 0 {
			detDebugStop()
			continue
		}
		systemstack(func() {
			print("detsched: stop ", i+1, " at t")
			if b := gp.bubble; b != nil {
				detPrintSeconds(b.now - detBubbleStart)
			} else {
				print("-")
			}
			pp := gp.m.p.ptr()
			print(" s", detP(pp).switches, " k", now, " goroutine ", gp.goid, " in ")
			if name, file, line := detSitePos(pc); name != "" {
				for i := len(file) - 1; i >= 0; i-- {
					if file[i] == '/' {
						file = file[i+1:]
						break
					}
				}
				print(name, " (", file, ":", line, ")")
			} else {
				print("?")
			}
			print(" trace=", hex(detMix(detMix(0, detP(pp).trace), detP(pp).switches)), "\n")
			if p.dump {
				goroutineheader(gp)
				traceback(pc, sp, 0, gp)
				tracebackothers(gp)
				print("\n")
			}
		})
		detStop(i + 1)
		if p.every != 0 {
			// The next stop of a periodic point: past where this one
			// fired, which may be past its point.
			var here uint64
			switch p.kind {
			case 's':
				here = detP(gp.m.p.ptr()).switches
			case 'k':
				here = uint64(now)
			case 't':
				if b := gp.bubble; b != nil {
					here = uint64(b.now - detBubbleStart)
				}
			}
			for p.n <= here {
				p.n += p.every
			}
			p.fired, p.armed = false, false
		}
	}
	detStopsNext()
	if real := detTicksReal(); real > 0 {
		detPutTicks(real)
	}
}

// detPrintSeconds prints ns as seconds with nine decimals.
func detPrintSeconds(ns int64) {
	print(ns/1e9, ".")
	for d := int64(1e8); d > 0; d /= 10 {
		print(ns / d % 10)
	}
}

// detStop is where a debugger breaks for stop i of GODETSTOP.
//
//go:noinline
//go:nosplit
func detStop(i int) {}
