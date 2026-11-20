// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/abi"
	"internal/goarch"
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// Garbage collection under GOEXPERIMENT=detsched.
//
// When the heap triggers a GC depends on how much the program allocates,
// which depends on how it was compiled: a build with -gcflags=all=-N -l
// moves more values to the heap than an optimized one. So a GC must not
// change the schedule, and in det mode it doesn't:
//
//   - The goroutine whose allocation started the cycle marks the heap
//     itself, on the system stack, and finishes the cycle before it goes
//     on (detGCMark), with no other goroutine running in between, and
//     with its budget of ticks as it was. No mark worker goroutines are
//     started, so goroutine IDs do not depend on when the first GC ran.
//   - What a program can see of a cycle waits for an epoch: finalizers
//     and cleanups queued, weak pointers cleared, and sync.Pools emptied.
//     A cycle that is not an epoch keeps every object with a finalizer,
//     a cleanup, or a weak pointer alive (see markrootSpans), and leaves
//     the pools alone (see clearpools). An epoch is a full GC at a point
//     the program's source decides: every call of runtime.GC, and the
//     first tick, in a goroutine that can run a GC, after the program set
//     detGCEpochSpecials finalizers, cleanups, and weak pointers since
//     the last epoch, which bounds the memory that waits for one. Not
//     after a number of ticks: the tick clock of a composer branch run in
//     place (GODETBRANCH) is not the forked branch's. So that a program
//     that waits for a finalizer or cleanup without calling runtime.GC
//     does not wait for good, an epoch also runs where no goroutine can
//     run and none waits for the OS, before the program idles or reports
//     a deadlock (detGCIdleEpoch).
//   - An epoch queues the finalizers and cleanups that are due in the
//     order they were set (detGCFlush), not in the order of the objects'
//     addresses, in which sweeping finds them.
//   - The random draws of GC work come from the M's state (mcheaprand),
//     not the P's, which decides what the program sees.
//
// A program can still see when cycles ran through runtime.ReadMemStats
// and similar statistics, and through the addresses of its heap objects,
// such as in the iteration order of a map keyed by pointers.
var detGC = struct {
	epoch    bool   // the running cycle is an epoch
	ran      bool   // a goroutine other than forcegc's ran since the last epoch
	next     int64  // the tick of the next epoch, or detStopNone
	specials uint32 // finalizers, cleanups and weak pointers set since the last epoch

	// While a cycle sweeps, the finalizer and cleanup specials of
	// the objects it frees wait in fins and cleanups, linked through
	// special.next, for detGCFlush. finSeq numbers finalizers in the
	// order they were set.
	sweeping       bool
	fins, cleanups *special
	finSeq         uint64

	// The due finalizers and cleanups set in a bubble, in the order
	// they were set, linked through special.next, which the finalizer
	// goroutine runs in their bubble (detGCDispatch), and where the
	// next of each goes.
	bubbleFins, bubbleCleanups         *special
	bubbleFinsTail, bubbleCleanupsTail **special
}{next: detStopNone}

// detFinalizer is the det-mode part of a specialfinalizer.
type detFinalizer struct {
	seq   uint64         // the order of the runtime.SetFinalizer call
	p     unsafe.Pointer // the object, once the finalizer is due
	where detGCTag
}

// A detGCTag says where a finalizer or cleanup was set: in which bubble,
// by its ID (the special is not in the GC'd heap, so it cannot hold the
// bubble), and on which simulated node, by the handle of its incarnation.
// Zero for none.
type detGCTag struct {
	bubble uint64
	node   int32
}

// detGCWhere returns the tag of a finalizer or cleanup the running
// goroutine sets.
func detGCWhere() detGCTag {
	gp := getg().m.curg
	if gp == nil || gp.bubble == nil {
		return detGCTag{}
	}
	return detGCTag{gp.bubble.id, detsimNode(gp)}
}

// detGCEpochSpecials is the number of finalizers, cleanups and weak
// pointers set that make an epoch due.
const detGCEpochSpecials = 4096

// detGCRegistered counts a finalizer, cleanup, or weak pointer the program
// set, and makes an epoch due at the next tick when there are enough.
//
//go:nosplit
func detGCRegistered() {
	detGC.specials++
	if detGC.specials >= detGCEpochSpecials && detGC.next > detTickNow()+1 {
		detGC.next = detTickNow() + 1
		detStopsNext()
		detPutTicks(detTicksReal())
	}
}

// detGCMark marks the heap after gcStart started a cycle, in the goroutine
// that started it, and finishes the cycle, as an assist that does all the
// work would (see gcAssistAlloc1).
func detGCMark() {
	gp := getg()
	for gcphase == _GCmark {
		systemstack(func() {
			if atomic.Xadd(&work.nwait, -1) == work.nproc {
				throw("detGCMark: nwait == nproc")
			}
			casGToWaitingForSuspendG(gp, _Grunning, waitReasonGCAssistMarking)
			gcDrain(&gp.m.p.ptr().gcw, 0)
			casgstatus(gp, _Gwaiting, _Grunning)
			if atomic.Xadd(&work.nwait, +1) > work.nproc {
				throw("detGCMark: nwait > nproc")
			}
		})
		gcMarkDone()
	}
}

// detGCAfterTimers starts the cycle that the allocations of the timers a
// bubble's clock just fired made due. They ran on the system stack, where
// gcStart starts none: the simulation's events, such as packets and
// keepalives, allocate there, and a run in which only they run, such as
// one whose goroutines wait while idle connections send keepalives for
// days of virtual time, would grow without bound. Called by synctestRun
// in the bubble's root goroutine, after the timers ran.
func detGCAfterTimers() {
	if t := (gcTrigger{kind: gcTriggerHeap}); t.test() {
		gcStart(t)
	}
}

// detGCEpochDue runs the epoch due at this tick in gp, if gp can run a GC
// here, and sets the tick of the next one. Called by detPreempt.
func detGCEpochDue(gp *g) {
	mp := gp.m
	if mp.locks != 0 || mp.mallocing != 0 || mp.preemptoff != "" || mp.p == 0 || mp.p.ptr().status != _Prunning {
		return // at a later tick
	}
	GC()
}

// detGCIdleEpoch makes the force-GC goroutine runnable, to call GC, and
// returns it for findRunnable to run, if an epoch is due where no
// goroutine can run and none waits for the OS (see detSynctestIdle): if
// the program has finalizers, cleanups, or weak pointers, which an epoch
// may run or clear, and another goroutine ran since the last epoch, which
// may have dropped their objects. The second condition stops a run that
// is deadlocked from calling GC again and again. Where no goroutine can
// run depends only on the schedule, so this keeps it independent of GC.
// Otherwise it returns nil.
func detGCIdleEpoch() *g {
	if !detGC.ran || forcegc.g == nil || !forcegc.idle.Load() {
		return nil
	}
	lock(&mheap_.speciallock)
	n := mheap_.specialfinalizeralloc.inuse + mheap_.specialCleanupAlloc.inuse + mheap_.specialWeakHandleAlloc.inuse
	unlock(&mheap_.speciallock)
	if n == 0 {
		return nil
	}
	detGC.ran = false
	lock(&forcegc.lock)
	forcegc.idle.Store(false)
	unlock(&forcegc.lock)
	gp := forcegc.g
	trace := traceAcquire()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.ok() {
		trace.GoUnpark(gp, 0)
		traceRelease(trace)
	}
	return gp
}

// detGCKeep marks the object with the special at offset in s, in a cycle
// that is not an epoch, so that its finalizer, cleanup, or weak pointers
// wait for one.
func detGCKeep(s *mspan, offset uintptr, gcw *gcWork) {
	if !goexperiment.DetSched || detGC.epoch {
		return
	}
	if obj, span, objIndex := findObject(s.base()+offset/s.elemsize*s.elemsize, 0, 0); obj != 0 {
		greyobject(obj, 0, 0, span, gcw, objIndex)
	}
}

// detGCPush pushes s on the list l, and returns the new list.
func detGCPush(l, s *special) *special {
	s.next = l
	return s
}

// detGCFlush queues the finalizers and cleanups that the sweep found due,
// in the order they were set, and frees their specials. Those set in a
// bubble wait for the finalizer goroutine instead, which runs them in
// their bubble (detGCDispatch): run outside it, a finalizer that closes a
// channel of the bubble or wakes one of its goroutines would end the
// program with a fatal error.
func detGCFlush() {
	fins := detGCSort(detGC.fins, func(s *special) uint64 {
		return (*specialfinalizer)(unsafe.Pointer(s)).det.seq
	})
	cleanups := detGCSort(detGC.cleanups, func(s *special) uint64 {
		return (*specialCleanup)(unsafe.Pointer(s)).id
	})
	detGC.fins, detGC.cleanups = nil, nil
	for s := fins; s != nil; {
		next := s.next
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		if sf.det.where.bubble != 0 {
			detGCDefer(&detGC.bubbleFins, &detGC.bubbleFinsTail, s)
			s = next
			continue
		}
		queuefinalizer(sf.det.p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
		s = next
	}
	for s := cleanups; s != nil; {
		next := s.next
		sc := (*specialCleanup)(unsafe.Pointer(s))
		if sc.det.bubble != 0 {
			detGCDefer(&detGC.bubbleCleanups, &detGC.bubbleCleanupsTail, s)
			s = next
			continue
		}
		gcCleanups.enqueue(sc.cleanup)
		lock(&mheap_.speciallock)
		mheap_.specialCleanupAlloc.free(unsafe.Pointer(sc))
		unlock(&mheap_.speciallock)
		s = next
	}
	if cleanups != nil {
		// The last sweeper flushed the queue before these.
		gcCleanups.flush()
	}
}

// detGCDefer appends the due finalizer or cleanup s, set in a bubble, to
// the list l, whose end is *tail, for the finalizer goroutine to run in
// its bubble (see detGCDispatch).
func detGCDefer(l *(*special), tail *(**special), s *special) {
	if *l == nil {
		*tail = l
	}
	s.next = nil
	**tail, *tail = s, &s.next
	fingStatus.Or(fingWake)
}

// detGCScanPending marks what the finalizers and cleanups waiting for
// detGCDispatch need, as markroot does the finalizer queue's.
func detGCScanPending(gcw *gcWork) {
	for s := detGC.bubbleFins; s != nil; s = s.next {
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		scanblock(uintptr(unsafe.Pointer(&sf.fn)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
		scanblock(uintptr(unsafe.Pointer(&sf.det.p)), goarch.PtrSize, &oneptrmask[0], gcw, nil)
	}
	for s := detGC.bubbleCleanups; s != nil; s = s.next {
		gcScanCleanup((*specialCleanup)(unsafe.Pointer(s)), gcw)
	}
}

// A detGCRecord is a due finalizer or cleanup, which detGCRunBubble runs.
type detGCRecord struct {
	next    *detGCRecord
	fin     finalizer // fn is nil for a cleanup
	cleanup cleanupFn
}

// detGCDispatch runs the due finalizers and cleanups that were set in a
// bubble, called by the finalizer goroutine, or by a bubble's root
// goroutine before its clock moves (see synctestRun). Those set on a
// simulated node whose incarnation has crashed or stopped are dropped, as
// a process that died runs none of its finalizers. Each of the others
// runs in a new goroutine of its bubble, on its node, one per bubble and
// node, in the order they were set: it can use the bubble's channels and
// mutexes, and the bubble does not idle, advance its clock, or report a
// deadlock until it ends; blocked for good, it is in the deadlock's
// report as any other goroutine. One whose bubble has ended runs as
// finalizers and cleanups set outside a bubble do.
func detGCDispatch() {
	type group struct {
		bubble *synctestBubble
		node   int32
		head   *detGCRecord
		tail   **detGCRecord
	}
	var groups []*group
	add := func(where detGCTag, r *detGCRecord) bool {
		if where.node != 0 && int(where.node) < len(detsim.state) && detsim.state[where.node] == detsimDead {
			return true // dropped
		}
		var bubble *synctestBubble
		for b := detBubbles; b != nil; b = b.detNext {
			if b.id == where.bubble {
				bubble = b
			}
		}
		if bubble == nil {
			return false
		}
		var g *group
		for _, gr := range groups {
			if gr.bubble == bubble && gr.node == where.node {
				g = gr
			}
		}
		if g == nil {
			g = &group{bubble: bubble, node: where.node}
			g.tail = &g.head
			groups = append(groups, g)
		}
		*g.tail, g.tail = r, &r.next
		return true
	}
	fins, cleanups := detGC.bubbleFins, detGC.bubbleCleanups
	detGC.bubbleFins, detGC.bubbleCleanups = nil, nil
	queued := false
	for s := fins; s != nil; {
		next := s.next
		sf := (*specialfinalizer)(unsafe.Pointer(s))
		r := &detGCRecord{fin: finalizer{fn: sf.fn, arg: sf.det.p, nret: sf.nret, fint: sf.fint, ot: sf.ot}}
		where := sf.det.where
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
		if !add(where, r) {
			queuefinalizer(r.fin.arg, r.fin.fn, r.fin.nret, r.fin.fint, r.fin.ot)
		}
		s = next
	}
	for s := cleanups; s != nil; {
		next := s.next
		sc := (*specialCleanup)(unsafe.Pointer(s))
		r := &detGCRecord{cleanup: sc.cleanup}
		where := sc.det
		lock(&mheap_.speciallock)
		mheap_.specialCleanupAlloc.free(unsafe.Pointer(sc))
		unlock(&mheap_.speciallock)
		if !add(where, r) {
			gcCleanups.enqueue(r.cleanup)
			queued = true
		}
		s = next
	}
	if queued {
		gcCleanups.flush()
	}
	gp := getg()
	f := detGCRunBubble
	fv := *(**funcval)(unsafe.Pointer(&f))
	pc := sys.GetCallerPC()
	bubble, spawn := gp.bubble, detG(gp).spawn
	for _, g := range groups {
		// The new goroutine takes its bubble and node from its creator,
		// as one Node.Go starts does.
		gp.bubble, detG(gp).spawn = g.bubble, g.node+1
		lock(&g.bubble.mu)
		// Where the root goroutine is not parked waiting for the
		// bubble to block, it woke already or is about to judge the
		// bubble blocked, so it must wait for the new goroutine (see
		// synctestRun). Where it is, the new goroutine's own blocking
		// wakes it.
		root := g.bubble.root
		if g.bubble.running == 0 || readgstatus(root)&^_Gscan != _Gwaiting || root.waitreason != waitReasonSynctestRun {
			g.bubble.detRejoined = true
		}
		g.bubble.detStarted++
		unlock(&g.bubble.mu)
		systemstack(func() {
			newg := newproc1(fv, gp, pc, false, waitReasonZero)
			detG(newg).records = g.head
			runqput(getg().m.p.ptr(), newg, true)
		})
		gp.bubble, detG(gp).spawn = bubble, spawn
	}
}

// detGCBeforeDeadlock runs a GC epoch in the root goroutine of bubble,
// before it reports a deadlock, if the program has finalizers, cleanups,
// or weak pointers and a goroutine ran since the last epoch, as
// detGCIdleEpoch does where no goroutine can run. It reports whether the
// epoch started goroutines in the bubble to run finalizers or cleanups,
// for which the root waits instead. Called with bubble.mu held, by the
// root goroutine.
func detGCBeforeDeadlock(bubble *synctestBubble) bool {
	lock(&mheap_.speciallock)
	n := mheap_.specialfinalizeralloc.inuse + mheap_.specialCleanupAlloc.inuse + mheap_.specialWeakHandleAlloc.inuse
	unlock(&mheap_.speciallock)
	if !detGC.ran || n == 0 {
		return false
	}
	detGC.ran = false
	started := bubble.detStarted
	unlock(&bubble.mu)
	GC()
	if detGCPendingIn(bubble) {
		detGCDispatch()
	}
	lock(&bubble.mu)
	bubble.detRejoined = false
	return bubble.detStarted != started
}

// detGCPendingIn reports whether finalizers or cleanups set in bubble
// wait for detGCDispatch.
func detGCPendingIn(bubble *synctestBubble) bool {
	for _, l := range [2]*special{detGC.bubbleFins, detGC.bubbleCleanups} {
		for s := l; s != nil; s = s.next {
			where := (*specialCleanup)(unsafe.Pointer(s)).det
			if s.kind == _KindSpecialFinalizer {
				where = (*specialfinalizer)(unsafe.Pointer(s)).det.where
			}
			if where.bubble == bubble.id {
				return true
			}
		}
	}
	return false
}

// detGCRunBubble runs the finalizers and cleanups detGCDispatch gave its
// goroutine, in order.
func detGCRunBubble() {
	gp := getg()
	r := detG(gp).records
	detG(gp).records = nil
	for ; r != nil; r = r.next {
		if r.fin.fn == nil {
			r.cleanup.call(r.cleanup.fn, r.cleanup.arg)
			continue
		}
		f := &r.fin
		var regs abi.RegArgs
		framesz := unsafe.Sizeof((any)(nil)) + f.nret
		frame := mallocgc(framesz, nil, true)
		p := frame
		if intArgRegs > 0 {
			p = unsafe.Pointer(&regs.Ints)
		}
		switch f.fint.Kind() {
		case abi.Pointer:
			*(*unsafe.Pointer)(p) = f.arg
		case abi.Interface:
			ityp := (*interfacetype)(unsafe.Pointer(f.fint))
			(*eface)(p)._type = &f.ot.Type
			(*eface)(p).data = f.arg
			if len(ityp.Methods) != 0 {
				(*iface)(p).tab = assertE2I(ityp, (*eface)(p)._type)
			}
		default:
			throw("bad type kind in finalizer")
		}
		reflectcall(nil, unsafe.Pointer(f.fn), frame, uint32(framesz), uint32(framesz), uint32(framesz), &regs)
	}
}

// detGCSort sorts the list l by key, with a merge sort, which needs no
// memory.
func detGCSort(l *special, key func(*special) uint64) *special {
	if l == nil || l.next == nil {
		return l
	}
	// Split l in two halves.
	slow, fast := l, l.next
	for fast != nil && fast.next != nil {
		slow, fast = slow.next, fast.next.next
	}
	b := slow.next
	slow.next = nil
	a := detGCSort(l, key)
	b = detGCSort(b, key)
	var head *special
	t := &head
	for a != nil && b != nil {
		if key(a) <= key(b) {
			*t, a = a, a.next
		} else {
			*t, b = b, b.next
		}
		t = &(*t).next
	}
	if a != nil {
		*t = a
	} else {
		*t = b
	}
	return head
}
