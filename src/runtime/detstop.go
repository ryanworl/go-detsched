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
// each optionally followed by ":dump". The run stops at the first tick at
// or after each point: the runtime prints
//
//	detsched: stop I at tD sN kN goroutine G
//
// to standard error, with the current position (t- outside a bubble),
// prints the stack of every goroutine for ":dump", and calls detStop(I),
// on which a debugger breaks: in a debugger, the frame below detPreempt
// is where goroutine G is. Then the run goes on, as if it had not
// stopped: the stops draw no randomness, run no code that counts ticks,
// and leave every goroutine's budget of ticks as it was.
//
// Ticks are the run's finest clock: the compiler counts one at every
// function entry and loop back edge in user code (see detsched.go), so no
// two hits of a breakpoint in user code have the same tick, and
// detTickEnd-int64(detTicks) is the tick now. A debugger that records it
// at a breakpoint can replay to just before that hit with GODETSTOP=kN.

// detStopNone is detStops.next when no stop is due.
const detStopNone = 1 << 62

// detBubbleStart is the time at which every synctest bubble starts,
// midnight UTC 2000-01-01.
const detBubbleStart = 946684800000000000

// A detStopPoint is a point of GODETSTOP.
type detStopPoint struct {
	kind  byte   // 't', 's', or 'k'
	n     uint64 // nanoseconds, switch, or tick
	dump  bool   // print every goroutine's stack
	armed bool   // reached: it stops at the next tick
	fired bool
}

var detStops = struct {
	points []detStopPoint
	next   int64 // the tick at which the next stop is due
}{next: detStopNone}

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
	s := gogetenv("GODETSTOP")
	if s == "" {
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
			switch p.kind {
			case 't':
				p.n, ok = detParseSeconds(item[1:])
			case 's', 'k':
				p.n, ok = atoi64u(item[1:])
				ok = ok && p.n < detStopNone
			default:
				ok = false
			}
		}
		if !ok {
			print("detsched: GODETSTOP point ", item, "\n")
			fatal("GODETSTOP must be a list of points tD, sN, or kN, each optionally followed by :dump")
		}
		points = append(points, p)
	}
	detStops.points = points
	detStopsNext()
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
	for i := range detStops.points {
		p := &detStops.points[i]
		if p.fired || p.armed {
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
	next := int64(detStopNone)
	for _, p := range detStops.points {
		switch {
		case p.fired:
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
	for i := range detStops.points {
		p := &detStops.points[i]
		if p.fired || !p.armed && !(p.kind == 'k' && int64(p.n) <= now) {
			continue
		}
		p.fired = true
		systemstack(func() {
			print("detsched: stop ", i+1, " at t")
			if b := gp.bubble; b != nil {
				ns := b.now - detBubbleStart
				print(ns/1e9, ".")
				for d := int64(1e8); d > 0; d /= 10 {
					print(ns / d % 10)
				}
			} else {
				print("-")
			}
			print(" s", detP(gp.m.p.ptr()).switches, " k", now, " goroutine ", gp.goid, "\n")
			if p.dump {
				goroutineheader(gp)
				traceback(pc, sp, 0, gp)
				tracebackothers(gp)
				print("\n")
			}
		})
		detStop(i + 1)
	}
	detStopsNext()
	if real := detTicksReal(); real > 0 {
		detPutTicks(real)
	}
}

// detStop is where a debugger breaks for stop i of GODETSTOP.
//
//go:noinline
//go:nosplit
func detStop(i int) {}
