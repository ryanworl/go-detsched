// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
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
}{next: detStopNone}

// detFinalizer is the det-mode part of a specialfinalizer.
type detFinalizer struct {
	seq uint64         // the order of the runtime.SetFinalizer call
	p   unsafe.Pointer // the object, once the finalizer is due
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
// in the order they were set, and frees their specials.
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
		queuefinalizer(sf.det.p, sf.fn, sf.nret, sf.fint, sf.ot)
		lock(&mheap_.speciallock)
		mheap_.specialfinalizeralloc.free(unsafe.Pointer(sf))
		unlock(&mheap_.speciallock)
		s = next
	}
	for s := cleanups; s != nil; {
		next := s.next
		sc := (*specialCleanup)(unsafe.Pointer(s))
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
