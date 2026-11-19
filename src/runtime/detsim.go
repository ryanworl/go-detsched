// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Simulation support for GOEXPERIMENT=detsched.
//
// A simulation, such as package detsched/sdk/sim, runs several nodes of a
// distributed system in one process and gives each its own simulated
// network stack and disk, so that unmodified code calling net.Listen,
// net.Dial, or os.OpenFile on a node reaches that node's simulated
// resources. The runtime provides the mechanisms; the simulation lives in
// ordinary Go code that registers itself with detsimRegister.
//
//   - Identity. Each goroutine carries a node handle (detGState.node), 0
//     outside every node. A new goroutine inherits its creator's handle,
//     and a goroutine started by a timer (time.AfterFunc) inherits the
//     handle of the goroutine that created the timer. A node's goroutines
//     share a handle until the node crashes; a restarted node gets a new
//     handle.
//
//   - System calls. syscall.Syscall, Syscall6, and RawSyscall (for the
//     traps the simulation lists) offer each call to the simulation first,
//     through syscall_detsimSyscall. The simulation's handler runs on the
//     system stack, because the arguments may point into the caller's
//     stack, which must not move; so it must not block, panic, or use much
//     stack. It sees the caller's node handle and decides whether to
//     handle the call: path and socket calls from a node, and calls on the
//     simulation's descriptors (detsimFD) and mappings (detsimMapAddr),
//     which work from any goroutine, such as a finalizer that unmaps a
//     mapping.
//
//   - Descriptors. Simulated descriptors are numbers at or above
//     detsimFDBase, which no real descriptor reaches. Sockets, from
//     detsimSockBase on, are registered with the network poller without the
//     OS: the simulation returns EAGAIN, the goroutine parks in
//     netpollblock as for a real socket, and the simulation wakes it with
//     detsimReady. Their deadlines use the bubble's fake clock. Files are
//     not pollable, like regular files on Linux.
//
//   - Time. detsimAfter runs the simulation's timer function at a time on
//     the fake clock of the calling goroutine's synctest bubble, which is
//     how messages arrive after a delay.
//
//   - Crashes. detsimCrash freezes every goroutine of a node: each one
//     leaves its bubble and never runs again; the scheduler parks a frozen
//     goroutine instead of running it (detsimFreeze). Its stack and heap
//     stay as they were, including locks it holds.
//
//   - Pauses. detsimPause stops a node's goroutines from running, as
//     SIGSTOP would: the scheduler parks each one it would run, in its
//     bubble, as durably blocked, and detsimResume makes them runnable
//     again. detsimCount counts a node's goroutines, so that a graceful
//     stop can wait for them to exit.
//
//   - Panics. A simulation may handle an unrecovered panic on a node's
//     goroutine (detsimPanic), as a crash of that node's process rather
//     than of the whole program.
//
//   - Clock skips. While a simulation runs, time.Now in a bubble returns
//     the bubble's time plus a skew the simulation sets, so the wall clock
//     can jump, and a monotonic reading from the bubble's clock, which the
//     skew leaves alone (detsimTime); stock bubbles omit the monotonic
//     reading.
//
//   - Faults. Each P has a third random stream, fault, seeded like the
//     others from GODETSEED and reseeded at each branch point of the tape,
//     so fault injection branches with the schedule. detsimEpoch changes
//     at every reseeding; buggify uses it to pick which fault sites are
//     enabled in a run (swarm testing).
//
//   - Watched callbacks. The simulation's fault injector (sim's Nemesis)
//     calls the harness's callbacks on its own goroutine, which alone
//     resumes the nodes it paused. A callback that waits for one of them,
//     such as for a lock that a goroutine of a paused node holds, waits
//     forever while the other nodes' timers move the clock on. The
//     runtime watches the goroutine (detsimWatch) and fails the program
//     at once if a callback is about to wait for a sync.Mutex or RWMutex
//     that a goroutine of a node the injector paused holds, and once a
//     callback has been blocked for detsimWatchSeconds of the bubble's
//     time.
//
//   - Failure log. The simulation may register a buffer that the runtime
//     writes to standard error when the run fails (detsimSetFailLog), such
//     as its timeline of faults.

package runtime

import (
	"internal/goexperiment"
	"internal/runtime/atomic"
	"unsafe"
)

// detsimMutexWaiterShift is internal/sync's mutexWaiterShift: a
// sync.Mutex's state counts its waiters from this bit up.
const detsimMutexWaiterShift = 3

const (
	detsimFDBase   = 1 << 24 // first simulated descriptor
	detsimSockBase = 1 << 29
	detsimFDLimit  = 1 << 30
)

// The simulation maps the memory of a mapped file at addresses from
// detsimMapBase on (sdk/sim's mapBase), above the heap and below where
// Linux places mappings.
const (
	detsimMapBase  = 0x5000 << 32
	detsimMapLimit = 0x6000 << 32
)

// detsim is the state of the registered simulation. Only the goroutine
// holding the single P touches it.
var detsim struct {
	// handler handles a system call for the goroutine with the given
	// node handle, reporting whether it did. It runs on the system stack.
	handler func(node int32, trap uintptr, args [6]uintptr) (r1, r2, errno uintptr, ok bool)
	// timer runs the event with the given ID. It runs on the system stack.
	timer func(id uint64)
	// rawTraps are the traps RawSyscall offers to the handler. RawSyscall
	// also runs in the child of vfork, so it offers nothing else. The
	// first, socket, is offered from goroutines of no node too, so that
	// the simulation can refuse them a real socket.
	rawTraps [4]uintptr
	// forked runs in the child of a fork before any goroutine does, on
	// the system stack, so that the simulation can separate the
	// child's shared memory from its parent's.
	forked func()

	state  []uint8 // by node handle: detsimLive, detsimPaused, or detsimDead
	frozen bool    // some node is paused or dead
	paused []*g    // goroutines parked because their node is paused

	pds     []*pollDesc // poll descriptors of simulated sockets
	heldPDs []*pollDesc // freed poll descriptors held back; see detsimPollAlloc
	epoch   uint64      // changes when the random streams are reseeded

	clock bool  // time.Now in a bubble uses detsimTime
	skew  int64 // added to the wall clock

	// panic handles an unrecovered panic with value v on a goroutine of
	// a node. If it returns, the panic ends the program.
	panic func(node int32, v any)

	// failLog is written to standard error when the run fails; see
	// detsimSetFailLog.
	failLog *[]byte

	// holders are the goroutines of crashed nodes that held a sync.Mutex
	// when their node crashed or still ran on (detsimSetAside), the only
	// ones of crashed nodes that can hold one, so that detsimSemacquire
	// need not scan every goroutine.
	holders []*g
}

// detsimWatched is the goroutine whose callbacks the runtime watches; see
// detsimWatch. It holds no pointers, so that it lives in noptrbss, which
// the GC pacer does not count as globals to scan: adding it changed no
// GC's timing, and so no schedule. Only the goroutine holding the single
// P touches it.
var detsimWatched struct {
	goid   uint64  // the goroutine, or 0
	bubble uintptr // its *synctestBubble
	calls  uintptr // its *uint32 that counts its callbacks
	node   uintptr // its node function's funcval, which is static
	now    int64   // the bubble's time when the watch last looked
	seq    uint32  // the callback running then
	start  int64   // when that callback started
}

// detsimWatchSeconds is how long a watched callback may be blocked, in
// seconds of its bubble's time; see detsimWatch.
const detsimWatchSeconds = 60

const (
	detsimLive = iota
	detsimPaused
	detsimDead
)

// detsimSetState sets the state of the node with the given handle. A dead
// handle stays dead: a restarted node gets a new one, and a pause or
// resume racing with the crash must not revive its goroutines.
func detsimSetState(node int32, state uint8) {
	for int(node) >= len(detsim.state) {
		detsim.state = append(detsim.state, detsimLive)
	}
	if detsim.state[node] == detsimDead {
		return
	}
	detsim.state[node] = state
	if state != detsimLive {
		detsim.frozen = true
	}
}

// detsimState returns the state of gp's node.
func detsimState(gp *g) uint8 {
	node := detsimNode(gp)
	if node == 0 || int(node) >= len(detsim.state) {
		return detsimLive
	}
	return detsim.state[node]
}

// detsimFD reports whether fd is a simulated descriptor.
//
//go:nosplit
func detsimFD(fd uintptr) bool {
	return fd >= detsimFDBase && fd < detsimFDLimit
}

// detsimMapAddr reports whether addr may be in a simulated mapping.
//
//go:nosplit
func detsimMapAddr(addr uintptr) bool {
	return uint64(addr) >= detsimMapBase && uint64(addr) < detsimMapLimit
}

// detsimNode returns gp's node handle.
//
//go:nosplit
func detsimNode(gp *g) int32 { return detG(gp).node }

// detsimNames holds the names package sim gives node handles, such as
// "n1 (start 2)", by handle.
var detsimNames []string

// detsimSetName names node handle node.
func detsimSetName(node int32, name string) {
	for int(node) >= len(detsimNames) {
		detsimNames = append(detsimNames, "")
	}
	detsimNames[node] = name
}

// detsimHeader adds gp's node to its traceback header, by the name package
// sim gave it, else by its handle, and the node's state if it crashed or
// is paused, so that the live goroutines stand out among the frozen ones
// of crashed nodes.
func detsimHeader(gp *g) {
	node := detsimNode(gp)
	if node == 0 {
		return
	}
	if int(node) < len(detsimNames) && detsimNames[node] != "" {
		print(", node ", detsimNames[node])
	} else {
		print(", node ", node)
	}
	switch detsimState(gp) {
	case detsimDead:
		if detG(gp).zombie != 0 {
			print(" crashed, running on to release a lock")
		} else {
			print(" crashed")
		}
	case detsimPaused:
		print(" paused")
	}
}

// detsimNewproc gives newg the node handle of its creator. Called by
// newproc1. System goroutines belong to no node.
func detsimNewproc(newg, callergp *g) {
	if !goexperiment.DetSched {
		return
	}
	node := int32(0)
	if !isSystemGoroutine(newg, false) {
		node = detsimNode(callergp)
		if s := detG(callergp).spawn; s != 0 {
			node = s - 1
		}
	}
	detG(newg).node, detG(newg).spawn = node, 0
	if node != 0 && detBusy.on {
		detBusyGrow(node)
	}
}

// detsimTimerNode returns a pointer to t's node handle, the handle of the
// goroutine that created it.
func detsimTimerNode(t *timer) *int32 {
	return (*int32)(unsafe.Pointer(&t.det))
}

// detsimTimerInit records in t, a new time.Timer, the node handle of the
// goroutine creating it.
func detsimTimerInit(t *timer) {
	if goexperiment.DetSched {
		*detsimTimerNode(t) = detsimNodeOf()
	}
}

// detsimTimerDead reports whether t, a time.Timer, was created by a
// goroutine of a crashed node. Its last tick removes such a ticker from
// its heap (see unlockAndRun), as a crash ends a process's timers: no
// goroutine of the node ever reads it again, and it would keep the
// bubble's clock jumping from tick to tick once the live goroutines are
// blocked, instead of letting synctest report the deadlock.
func detsimTimerDead(t *timer) bool {
	if !detsim.frozen {
		return false
	}
	node := *detsimTimerNode(t)
	return node != 0 && int(node) < len(detsim.state) && detsim.state[node] == detsimDead
}

// detsimTimerEnter gives the goroutine running t's function the node
// handle of t's creator, so that a goroutine the function starts
// (time.AfterFunc) belongs to that node. It returns the handle to restore
// with detsimTimerExit.
func detsimTimerEnter(t *timer) int32 {
	if !goexperiment.DetSched {
		return 0
	}
	gp := getg()
	old := detG(gp).node
	detG(gp).node = *detsimTimerNode(t)
	return old
}

func detsimTimerExit(old int32) {
	if goexperiment.DetSched {
		detG(getg()).node = old
	}
}

// detsimIOWait returns the reason a goroutine waiting on pd parks with.
// A simulated socket is woken only by the simulation, in the bubble, so
// its waiters count as durably blocked.
func detsimIOWait(pd *pollDesc) waitReason {
	if goexperiment.DetSched && detsimFD(pd.fd) {
		return waitReasonDetSimIO
	}
	return waitReasonIOWait
}

func detsimRegister(handler func(int32, uintptr, [6]uintptr) (uintptr, uintptr, uintptr, bool), timer func(uint64), rawTraps [4]uintptr) {
	detsim.handler, detsim.timer, detsim.rawTraps = handler, timer, rawTraps
}

func detsimSetForked(f func()) {
	detsim.forked = f
}

// syscall_detsimSyscall offers a system call to the simulation. It must
// not grow the stack: the arguments may point into it.
//
//go:linkname syscall_detsimSyscall syscall.detsimSyscall
//go:nosplit
func syscall_detsimSyscall(trap, a1, a2, a3, a4, a5, a6 uintptr, raw bool) (r1, r2, errno uintptr, ok bool) {
	if detsim.handler == nil {
		return
	}
	if raw && trap != detsim.rawTraps[0] && trap != detsim.rawTraps[1] && trap != detsim.rawTraps[2] && trap != detsim.rawTraps[3] {
		return
	}
	gp := getg()
	if gp != gp.m.curg {
		return
	}
	node := detG(gp).node
	if node == 0 && !detsimFD(a1) && !detsimFD(a3) && trap != detsim.rawTraps[0] && !detsimMapAddr(a1) {
		return
	}
	args := [6]uintptr{a1, a2, a3, a4, a5, a6}
	systemstack(func() {
		// On the system stack, where the caller's nosplit chain leaves
		// room for them, as golang.org/x/sys/unix's jump to
		// syscall.Syscall6 needs in a build without optimizations.
		ticks, q, last := detsimEnter()
		r1, r2, errno, ok = detsim.handler(node, trap, args)
		detsimExit(ticks, q, last)
	})
	if detG(gp).wait != 0 {
		detsimWaitDevice(gp)
	}
	return
}

// detsimWait makes the running goroutine wait ns of bubble time when the
// system call the simulation is handling returns, as a device would make
// it wait. The simulation calls it from its handler.
//
//go:nosplit
func detsimWait(ns int64) {
	if gp := getg().m.curg; gp != nil && detG(gp).node != 0 && ns > 0 {
		detG(gp).wait += ns
	}
}

// detsimWaitDevice sleeps gp, which is running and has just returned from
// a system call the simulation handled, for the wait the simulation set
// (detsimWait), or until detsimWake ends it. The call's arguments are no
// longer in use, so its stack may grow.
func detsimWaitDevice(gp *g) {
	ns := detG(gp).wait
	detG(gp).wait = 0
	if gp.bubble == nil {
		return
	}
	detG(gp).waiting = true
	timeSleep(ns)
	detG(gp).waiting = false
}

// detsimWake ends now the waits (detsimWait) of the goroutines of the node
// with the given handle. Its caller is in their bubble.
func detsimWake(node int32) {
	var gs []*g
	systemstack(func() {
		forEachG(func(gp *g) {
			if detG(gp).node == node && detG(gp).waiting && readgstatus(gp)&^_Gscan == _Gwaiting && gp.waitreason == waitReasonSleep {
				gs = append(gs, gp)
			}
		})
	})
	for _, gp := range gs {
		gp.timer.reset(gp.bubble.now, 0)
	}
}

// net_runtime_detsimNode gives package net the calling goroutine's node
// handle, so that simulated nodes never share a pending DNS lookup.
//
//go:linkname net_runtime_detsimNode net.runtime_detsimNode
func net_runtime_detsimNode() int32 { return detsimNodeOf() }

func detsimNodeOf() int32 {
	gp := getg()
	if gp.m.curg != nil {
		gp = gp.m.curg
	}
	return detsimNode(gp)
}

// detsimSetNode sets the node handle of the goroutines the running
// goroutine starts from now on, and returns the previous setting to restore
// it with, -1 meaning the goroutine's own node. The simulation uses it to
// start a goroutine on another node. The running goroutine stays on its own
// node: it may yield before it restores the setting, and a crash of its
// node must still freeze it, and a pause of the other node must not.
func detsimSetNode(node int32) int32 {
	gp := getg()
	old := detG(gp).spawn - 1
	detG(gp).spawn = node + 1
	return old
}

// detsimCall runs f on the system stack, where nothing can preempt it, so
// the simulation's state never changes while it is half updated.
func detsimCall(f func()) {
	ticks, q, last := detsimEnter()
	systemstack(func() { f() })
	detsimExit(ticks, q, last)
}

// detsimCrash freezes the goroutines of the node with the given handle,
// which the simulation never reuses. The running goroutine, if it belongs
// to the node, leaves its bubble too and must park for good (detsimHalt).
func detsimCrash(node int32) {
	systemstack(func() {
		detsimSetState(node, detsimDead)
		detsimUnpause(node)
		// Collect first: leaving a bubble may ready its root goroutine,
		// which must not happen while forEachG holds allglock.
		var gs []*g
		forEachG(func(gp *g) {
			if detG(gp).node == node && readgstatus(gp)&^_Gscan != _Gdead {
				gs = append(gs, gp)
			}
		})
		for _, gp := range gs {
			status := readgstatus(gp) &^ _Gscan
			detsimLeaveBubble(gp, status)
			s := detG(gp)
			if status == _Gwaiting && gp.waitreason == waitReasonDetSimPaused {
				// It waits for a resume that never comes: frozen,
				// so that detsimThaw can run it on if it holds a
				// mutex. It left its bubble as a paused goroutine.
				gp.waitreason = waitReasonZero
				s.frozen = true
			}
			if s.heldGoid == gp.goid && s.nheld > 0 || s.aside {
				detsim.holders = append(detsim.holders, gp)
			}
			// Live goroutines that already wait for a mutex it holds
			// never call detsimSemacquire again: thaw it now. Readers
			// and writers of an RWMutex it holds wait on its readerSem
			// and writerSem (see detsimSemacquire). Waiters of the
			// crashed node need no thaw, and a thawed holder may block
			// for good on goroutines that stay frozen.
			for i := 0; s.heldGoid == gp.goid && i < int(s.nheld); i++ {
				l := s.held[i].l
				waiters := atomic.Load((*uint32)(unsafe.Pointer(l)))>>detsimMutexWaiterShift != 0 ||
					semtable.rootFor((*uint32)(unsafe.Pointer(l+8))).nwait.Load() != 0 ||
					semtable.rootFor((*uint32)(unsafe.Pointer(l+12))).nwait.Load() != 0
				if waiters && detsimLiveWaiter(l) && detsimThaw(gp, l) {
					break
				}
			}
		}
	})
}

// detsimHalt parks the running goroutine for good. A goroutine calls it
// after crashing its own node.
func detsimHalt() {
	gopark(nil, nil, waitReasonZero, traceBlockForever, 1)
}

// detsimLeaveBubble removes gp, with status status, from its bubble, so
// that the bubble no longer waits for it.
func detsimLeaveBubble(gp *g, status uint32) {
	b := gp.bubble
	if b == nil {
		// A frozen goroutine never runs again to take its bubble back.
		b = detG(gp).bubble
		detG(gp).bubble = nil
	}
	if b != nil && gp != b.main {
		b.changegstatus(gp, status, _Gdead)
		gp.bubble = nil
		detG(gp).left = b
	}
}

// detsimRejoin puts gp, a goroutine of a crashed node with status status,
// back in the bubble it left, while it runs as a zombie: it may use the
// bubble's channels and timers, and the bubble waits for it.
func detsimRejoin(gp *g, status uint32) {
	s := detG(gp)
	if b := s.left; b != nil && gp.bubble == nil && !s.aside {
		gp.bubble = b
		b.changegstatus(gp, _Gdead, status)
	}
}

// detsimSetAside records b as the bubble of gp, the running goroutine,
// while gcStart or gcAssistAlloc sets gp.bubble aside, and nil when they
// put it back. The bubble still counts gp. If gp's node crashes or pauses
// meanwhile, gp runs on, as gcStart may hold the runtime's GC semaphores,
// which every node's goroutines need, until it puts the bubble back and
// stops there: detsimLeaveBubble and detsimResume account for gp in b.
func detsimSetAside(gp *g, b *synctestBubble) {
	if !goexperiment.DetSched {
		return
	}
	s := detG(gp)
	// A crash while gp was aside took it out of its bubble and cleared
	// s.bubble (detsimLeaveBubble); a zombie that detsimRejoin put back
	// before it came here is still counted.
	counted := s.bubble != nil
	s.bubble, s.aside = b, b != nil
	if b != nil || !detsim.frozen {
		return
	}
	switch detsimState(gp) {
	case detsimDead:
		if !counted {
			gp.bubble = nil
		}
		if s.zombie == 0 {
			systemstack(func() { detsimLeaveBubble(gp, _Grunning) })
			s.frozen = true
			detsimHalt()
		}
	case detsimPaused:
		systemstack(func() { detsim.paused = append(detsim.paused, gp) })
		gopark(nil, nil, waitReasonDetSimPaused, traceBlockForever, 1)
	}
}

// detsimAsideBubble returns gp's bubble if gcStart or gcAssistAlloc set
// it aside, and nil otherwise.
func detsimAsideBubble(gp *g) *synctestBubble {
	if gp.bubble != nil {
		return nil
	}
	return detG(gp).bubble
}

// detsimFreeze is called by schedule with the goroutine it is about to
// run. If gp belongs to a crashed node, it parks gp for good, outside its
// bubble; if gp's node is paused, it parks gp until the node resumes. It
// reports whether it parked gp. It may have write barriers: schedule holds
// a P when it calls it.
//
//go:yeswritebarrierrec
func detsimFreeze(gp *g) bool {
	state := detsimState(gp)
	if state == detsimLive || gp.lockedm != 0 {
		return false
	}
	s := detG(gp)
	if s.aside {
		return false // see detsimSetAside
	}
	if state == detsimDead && s.zombie != 0 {
		// A zombie (see detsimSemacquire) runs on, in its bubble; it
		// is out of it if it was thawed or woke while it waited.
		detsimRejoin(gp, _Grunnable)
		return false
	}
	if state == detsimDead && s.woke != 0 && detsimLiveWaiter(s.woke) {
		// See detsimSemrelease. Once it takes the mutex it may hold
		// others when it freezes.
		s.zombie, s.woke = s.woke, 0
		detsimRejoin(gp, _Grunnable)
		detsim.holders = append(detsim.holders, gp)
		return false
	}
	if state == detsimDead {
		detsimLeaveBubble(gp, _Grunnable)
		gp.waitreason = waitReasonZero
		s.frozen = true
		if s.woke != 0 {
			// Only goroutines of its crashed node wait for the
			// mutex it was woken for: it keeps the turn frozen, as
			// it would touch its dead process's memory, until a
			// live goroutine waits for the mutex and thaws it
			// (detsimSemacquire).
			detsim.holders = append(detsim.holders, gp)
		}
	} else {
		gp.waitreason = waitReasonDetSimPaused
		detsim.paused = append(detsim.paused, gp)
		if b := detsimAsideBubble(gp); b != nil {
			b.changegstatus(gp, _Grunnable, _Gwaiting)
		}
	}
	casgstatus(gp, _Grunnable, _Gwaiting)
	return true
}

// detsimPause pauses the node with the given handle. If the running
// goroutine belongs to it, it parks at once.
func detsimPause(node int32) {
	systemstack(func() { detsimSetState(node, detsimPaused) })
	if gp := getg(); detsimNode(gp) == node {
		if detsimState(gp) == detsimDead {
			detsimHalt() // detsimCrash already took it out of its bubble
		}
		systemstack(func() { detsim.paused = append(detsim.paused, gp) })
		gopark(nil, nil, waitReasonDetSimPaused, traceBlockForever, 1)
	}
}

// detsimResume makes the goroutines of the paused node with the given
// handle runnable again.
func detsimResume(node int32) {
	systemstack(func() {
		if int(node) < len(detsim.state) && detsim.state[node] == detsimDead {
			return // detsimCrash froze its goroutines for good
		}
		detsimSetState(node, detsimLive)
		for _, gp := range detsimUnpause(node) {
			if b := detsimAsideBubble(gp); b != nil {
				b.changegstatus(gp, _Gwaiting, _Grunnable)
			}
			goready(gp, 0)
		}
	})
}

// detsimUnpause removes the goroutines of node from the paused list and
// returns them.
func detsimUnpause(node int32) []*g {
	var gs []*g
	kept := detsim.paused[:0]
	for _, gp := range detsim.paused {
		if detsimNode(gp) == node {
			gs = append(gs, gp)
		} else {
			kept = append(kept, gp)
		}
	}
	clear(detsim.paused[len(kept):])
	detsim.paused = kept
	return gs
}

// detsimCount returns the number of goroutines of the node with the given
// handle.
func detsimCount(node int32) int {
	n := 0
	systemstack(func() {
		forEachG(func(gp *g) {
			if detG(gp).node == node && readgstatus(gp)&^_Gscan != _Gdead {
				n++
			}
		})
	})
	return n
}

// detsimStacks writes the stacks of the goroutines of the node with the
// given handle to buf, as Stack does for every goroutine, and returns the
// number of bytes written.
func detsimStacks(node int32, buf []byte) int {
	stw := stopTheWorld(stwAllGoroutinesStack)
	me := getg()
	n := 0
	systemstack(func() {
		g0 := getg()
		g0.m.traceback = 1
		g0.writebuf = buf[0:0:len(buf)]
		tracebacksomeothers(me, func(gp *g) bool { return detG(gp).node == node })
		g0.m.traceback = 0
		n = len(g0.writebuf)
		g0.writebuf = nil
	})
	startTheWorld(stw)
	return n
}

// detsimAfter runs the simulation's timer function with id after delay
// nanoseconds (at least 1) of the current bubble's fake time.
func detsimAfter(delay int64, id uint64) {
	systemstack(func() {
		gp := getg()
		b := detsimBubble()
		if b == nil {
			// Such as a simulated socket closed by a finalizer, which
			// runs outside every bubble: the event is dropped, so the
			// peer of a leaked connection never hears of its close.
			return
		}
		saved := gp.bubble
		gp.bubble = b
		t := new(timer)
		t.init(nil, nil)
		t.isFake = true
		t.modify(b.now+max(delay, 1), 0, detsimTimerFired, id, 0)
		gp.bubble = saved
	})
}

func detsimTimerFired(arg any, _ uintptr, _ int64) {
	id := arg.(uint64)
	ticks, q, last := detsimEnter()
	systemstack(func() { detsim.timer(id) })
	detsimExit(ticks, q, last)
}

// detsimBubble returns the bubble of the running goroutine, also on the
// system stack.
func detsimBubble() *synctestBubble {
	gp := getg()
	if gp.bubble == nil && gp.m.curg != nil {
		gp = gp.m.curg
	}
	return gp.bubble
}

func detsimNow() int64 {
	if b := detsimBubble(); b != nil {
		return b.now
	}
	return 0
}

// detsimReady makes the simulated socket fd ready for mode ('r' or 'w'),
// waking a goroutine waiting for it.
func detsimReady(fd uintptr, mode int32) {
	for _, pd := range detsim.pds {
		if pd.fd != fd {
			continue
		}
		delta := int32(0)
		lock(&pd.lock)
		gp := netpollunblock(pd, mode, true, &delta)
		unlock(&pd.lock)
		if gp != nil {
			goready(gp, 0)
		}
		netpollAdjustWaiters(delta)
		return
	}
}

// detsimPollAlloc returns a poll descriptor from pollcache, starting with
// pd, whose deadline timers can serve a descriptor that is a simulated
// socket's (fake) or not. A timer that a Stop left in a heap stays in that
// heap when it is modified again, and a simulated socket's timers use the
// bubble's fake clock, so a descriptor whose timers are still in a heap of
// the other kind, or of another bubble, is held back until they leave it.
func detsimPollAlloc(pd *pollDesc, fake bool) *pollDesc {
	held := detsim.heldPDs[:0]
	for _, p := range detsim.heldPDs {
		if detsimPollFree(p) {
			pollcache.free(p)
		} else {
			held = append(held, p)
		}
	}
	for i := len(held); i < len(detsim.heldPDs); i++ {
		detsim.heldPDs[i] = nil
	}
	detsim.heldPDs = held
	var ts *timers
	if b := getg().bubble; fake && b != nil {
		ts = &b.timers
	}
	for !detsimPollUsable(pd, fake, ts) {
		// On the system stack, where allocating never parks the goroutine.
		systemstack(func() { detsim.heldPDs = append(detsim.heldPDs, pd) })
		pd = pollcache.alloc()
	}
	return pd
}

// detsimPollUsable reports whether pd's timers are in no heap, or in the
// heap ts of the kind fake says.
func detsimPollUsable(pd *pollDesc, fake bool, ts *timers) bool {
	for _, t := range [...]*timer{&pd.rt, &pd.wt} {
		t.lock()
		heaped, tts := t.state&timerHeaped != 0, t.ts
		t.unlock()
		if heaped && (t.isFake != fake || fake && tts != ts) {
			return false
		}
	}
	return true
}

// detsimPollFree reports whether pd's timers have left every heap.
func detsimPollFree(pd *pollDesc) bool {
	for _, t := range [...]*timer{&pd.rt, &pd.wt} {
		t.lock()
		heaped := t.state&timerHeaped != 0
		t.unlock()
		if heaped {
			return false
		}
	}
	return true
}

// detsimPollOpen registers pd for the simulated descriptor fd with the
// poller in place of netpollopen (see poll_runtime_pollOpen). Files are
// not pollable.
func detsimPollOpen(fd uintptr, pd *pollDesc) (*pollDesc, int) {
	if fd < detsimSockBase {
		pollcache.free(pd)
		return nil, 1 // EPERM
	}
	pd.rt.isFake = true
	pd.wt.isFake = true
	// Append on the system stack, where allocating never parks the
	// goroutine for a GC assist: another goroutine could then close a
	// socket, and the append would undo the removal and leave a nil.
	systemstack(func() { detsim.pds = append(detsim.pds, pd) })
	return pd, 0
}

// detsimPollClose unregisters and frees pd, a simulated socket's, in
// place of netpollclose (see poll_runtime_pollClose).
func detsimPollClose(pd *pollDesc) {
	pd.rt.isFake = false
	pd.wt.isFake = false
	for i, p := range detsim.pds {
		if p == pd {
			last := len(detsim.pds) - 1
			detsim.pds[i] = detsim.pds[last]
			detsim.pds[last] = nil
			detsim.pds = detsim.pds[:last]
			break
		}
	}
	pollcache.free(pd)
}

// detsimDeadline returns the deadline d nanoseconds from now on the fake
// clock, for a simulated socket's poll descriptor (see
// poll_runtime_pollSetDeadline).
func detsimDeadline(d int64) int64 {
	if d <= 0 {
		return d
	}
	d += detsimNow()
	if d <= 0 {
		d = 1<<63 - 1
	}
	return d
}

// detsimRealPollWaiters reports whether a goroutine waits on the network
// poller for a real descriptor. Goroutines waiting for simulated sockets
// are woken only by the simulation, so they block like goroutines waiting
// on a channel.
func detsimRealPollWaiters() bool {
	n := netpollWaiters.Load()
	for _, pd := range detsim.pds {
		if pd.rg.Load() > pdWait {
			n--
		}
		if pd.wg.Load() > pdWait {
			n--
		}
	}
	return n > 0
}

// detsimSeed seeds pp's fault stream from the key (a, b); see
// detSeedStreams.
func detsimSeed(pp *p, a, b uint64) {
	detP(pp).fault.Init64([4]uint64{a, b, 0x6465747363686564, 2})
	detsim.epoch = detMix(detMix(a, b), 0x6661756c74) // "fault"
}

// detsimRand returns the next value of the fault stream.
func detsimRand() uint64 {
	if !goexperiment.DetSched {
		return 0
	}
	c := &detP(getg().m.p.ptr()).fault
	for {
		if x, ok := c.Next(); ok {
			return x
		}
		mp := acquirem()
		c.Refill()
		releasem(mp)
	}
}

func detsimEpoch() uint64 { return detsim.epoch }

func detsimBuggifyRand() uint64 { return detsimRand() }

func detsimBuggifyEpoch() uint64 { return detsim.epoch }

func detsimBuggifyCall(f func())             { detsimCall(f) }
func detsimBuggifyFaultOff(site string) bool  { return detFaultOff("", site) }

func detsimSetClock(on bool, skew int64) { detsim.clock, detsim.skew = on, skew }

// detsimStartNano is package time's startNano, from which time.Time
// stores monotonic readings as offsets (see dettime.go).
var detsimStartNano int64

// detsimTime is time.Now in a bubble while a simulation runs: the wall
// clock shifted by the simulation's skew, and a monotonic reading from the
// bubble's clock, so durations measured with time.Since ignore the skew.
// The reading is offset by startNano, which depends on when the program
// started, so that the offset time.Time stores is the bubble's clock and
// the same in every run, as are times printed with it, compared with ==,
// or hashed as map keys.
func detsimTime(bubble *synctestBubble) (sec int64, nsec int32, mono int64) {
	t := bubble.now + detsim.skew
	return t / 1e9, int32(t % 1e9), bubble.now + detsimStartNano
}

// detsimSemacquire is called when the calling goroutine, of a live node or
// of none, is about to wait for a sync.Mutex. If a goroutine of a crashed
// node holds it, the waiter would wait forever, as no real crash can make
// it, because the nodes share memory, such as caches in libraries that
// separate processes would each have a copy of. So the holder runs on, as
// a zombie, until it releases the mutex, and then freezes again; a holder
// that cannot run on fails the program. Mutexes are tracked while a
// simulation is registered (internal_sync_runtime_detLocked); the semaphore of a
// sync.Mutex is its second word. A sync.RWMutex's holders, a writer and
// readers, are recorded with the address of its first field, its
// sync.Mutex w: a reader waits on its readerSem, the fourth word, for a
// writer, and a writer on its writerSem, the third, for the readers.
func detsimSemacquire(addr *uint32, reason waitReason) {
	gp := getg()
	var l uintptr
	switch reason {
	case waitReasonSyncMutexLock:
		l = uintptr(unsafe.Pointer(addr)) - 4
	case waitReasonSyncRWMutexRLock:
		l = uintptr(unsafe.Pointer(addr)) - 12
	case waitReasonSyncRWMutexLock:
		l = uintptr(unsafe.Pointer(addr)) - 8
	default:
		return
	}
	if detsimState(gp) != detsimLive {
		if s := detG(gp); reason == waitReasonSyncMutexLock && detsimState(gp) == detsimDead && s.zombie == l {
			// A zombie that a wake let contend for the mutex lost it
			// (see detsimSemrelease): it no longer holds the turn,
			// and sleeps again outside its bubble, as frozen
			// goroutines do, until another wake.
			s.zombie = 0
			detsimLeaveBubble(gp, _Grunning)
		}
		return
	}
	if detsimWatched.goid == gp.goid && detsimWatching() {
		detsimWatchLock(gp, l, reason)
	}
	// Several goroutines can seem to hold it: one that froze after it
	// released the mutex but before it recorded that keeps an entry for
	// it, as does a holder of a mutex that another goroutine unlocked.
	// Each one runs on.
	var holders [8]*g
	n := 0
	keep := detsim.holders[:0]
	for _, hg := range detsim.holders {
		s := detG(hg)
		holds := s.heldGoid == hg.goid && s.nheld > 0
		if !holds && !s.aside && s.zombie == 0 && s.woke == 0 || detsimState(hg) != detsimDead {
			continue // it released its mutexes for good, or its g was reused
		}
		keep = append(keep, hg)
		if s.woke == l && s.frozen && n < len(holders) {
			holders[n] = hg // frozen with the mutex's turn; see detsimFreeze
			n++
			continue
		}
		for i := 0; holds && n < len(holders) && i < int(s.nheld); i++ {
			if s.held[i].l == l {
				holders[n] = hg
				n++
				break
			}
		}
	}
	clear(detsim.holders[len(keep):])
	detsim.holders = keep
	for _, holder := range holders[:n] {
		if detsimThaw(holder, l) {
			continue
		}
		print("detsim: goroutine ", gp.goid, " (node ", detsimNode(gp), ") waits for a sync.Mutex held by goroutine ", holder.goid, " of crashed node ", detsimNode(holder), "\n")
		fatal("detsim: a goroutine waits for a mutex that a crashed node holds; nodes must share no memory except through the simulation")
	}
}

// detsimLiveWaiter reports whether a goroutine of a node that has not
// crashed, or of none, waits on the semaphore of the sync.Mutex at l,
// its second word, or on the readerSem or writerSem of the RWMutex
// whose w is at l.
func detsimLiveWaiter(l uintptr) bool {
	found := false
	forEachG(func(gp *g) {
		if found || readgstatus(gp)&^_Gscan != _Gwaiting || gp.waiting == nil || detsimState(gp) == detsimDead {
			return
		}
		switch e := gp.waiting.elem.uintptr(); gp.waitreason {
		case waitReasonSyncMutexLock:
			found = e == l+4
		case waitReasonSyncRWMutexRLock:
			found = e == l+12
		case waitReasonSyncRWMutexLock:
			found = e == l+8
		}
	})
	return found
}

// detsimThaw has holder, a goroutine of a crashed node that holds the
// sync.Mutex at l, run on as a zombie until it releases it: it thaws a
// holder frozen for good, and lets one that is runnable or waits for
// something else run when it can. A waiting holder stays out of its
// bubble until it runs (detsimFreeze), as what it waits for may never
// come. It reports false for a holder that crashed its own node
// (detsimHalt), which never runs again.
func detsimThaw(holder *g, l uintptr) bool {
	s := detG(holder)
	status := readgstatus(holder) &^ _Gscan
	switch {
	case s.zombie != 0:
		return true
	case s.frozen && status == _Gwaiting:
		s.zombie, s.frozen = l, false
		if s.woke == l {
			s.woke = 0
		}
		detsimRejoin(holder, status)
		goready(holder, 0)
		return true
	case status == _Grunnable:
		s.zombie = l
		detsimRejoin(holder, status)
		return true
	case status == _Gwaiting && holder.waitreason != waitReasonZero:
		s.zombie = l
		return true
	}
	return false
}

// detsimZombieWait is called with gp, the running goroutine, before it
// waits on a semaphore, or parks for anything else. If gp is a zombie
// (see detsimSemacquire), what it waits for, such as RWMutex readers or
// a reply from a goroutine of its node, may be frozen for good: it
// leaves its bubble, so that the bubble does not wait for it, and
// reports true; gp must rejoin the bubble (detsimRejoin) once it runs
// again, as detsimFreeze has it do.
func detsimZombieWait(gp *g) bool {
	s := detG(gp)
	if s.zombie == 0 || s.aside || gp.bubble == nil || detsimState(gp) != detsimDead {
		return false
	}
	systemstack(func() { detsimLeaveBubble(gp, _Grunning) })
	return true
}

// detsimSemrelease is called with gp, a goroutine that a release of the
// semaphore at addr is about to wake. A goroutine that a sync.Mutex's
// semaphore wakes, or that takes the semaphore without waiting, owns the
// mutex (in starvation mode) or the turn to contend for it, which keeps
// Unlock from waking others, until it takes the mutex; if its node
// crashes before then, detsimFreeze runs it on as a zombie, rather than
// keep the mutex from the live goroutines for good.
func detsimSemrelease(gp *g, addr *uint32) {
	if gp.waitreason == waitReasonSyncMutexLock {
		detG(gp).woke = uintptr(unsafe.Pointer(addr)) - 4
	}
}

// detsimWatch has the runtime watch the callbacks that the running
// goroutine, a simulation's fault injector (package sim's Nemesis), runs
// until it calls detsimWatch(nil, nil). *calls counts the callbacks begun
// and ended, so it is odd while one runs; the goroutine keeps it alive.
// node, which must be a top-level function, as the runtime keeps its
// funcval as a uintptr, names a node and says whether the goroutine
// paused it, and for node 0 names the callback running. One goroutine is
// watched at a time.
//
// The injector alone resumes the nodes it paused, so a callback must not
// wait for them. detsimSemacquire fails the program when a callback is
// about to wait for a sync.Mutex or RWMutex that a goroutine of such a
// node holds (detsimWatchLock), and synctestRun once the bubble's clock
// has moved detsimWatchSeconds since the callback started
// (detsimWatchJump). Until it fails, the
// watch adds no allocation, no pointer, no tick, and no goroutine or
// timer, and runs the simulation's code without ticks, so it changes no
// schedule.
func detsimWatch(calls *uint32, node func(unsafe.Pointer, int32) (string, bool)) {
	gp := getg()
	w := &detsimWatched
	if calls == nil {
		if w.goid == gp.goid {
			w.goid = 0
		}
		return
	}
	if w.goid != 0 || gp.bubble == nil {
		return
	}
	w.goid, w.bubble, w.calls = gp.goid, uintptr(unsafe.Pointer(gp.bubble)), uintptr(unsafe.Pointer(calls))
	w.node = *(*uintptr)(unsafe.Pointer(&node))
	w.now, w.seq = gp.bubble.now, 0
}

// detsimWatchCalls returns the watched goroutine's count of callbacks.
func detsimWatchCalls() uint32 {
	return *(*uint32)(unsafe.Pointer(detsimWatched.calls))
}

// detsimWatching reports whether the watched goroutine runs a callback.
func detsimWatching() bool { return detsimWatchCalls()&1 != 0 }

// detsimWatchNode calls the watched goroutine's node function (see
// detsimWatch) without ticks, as detsimCall does.
func detsimWatchNode(node int32) (name string, paused bool) {
	w := &detsimWatched
	f := *(*func(unsafe.Pointer, int32) (string, bool))(unsafe.Pointer(&w.node))
	ticks, q, last := detsimEnter()
	name, paused = f(unsafe.Pointer(w.calls), node)
	detsimExit(ticks, q, last)
	return
}

// detsimWatchPrintCall prints which callback the watched goroutine runs.
func detsimWatchPrintCall() {
	name, _ := detsimWatchNode(0)
	print("the nemesis's ", name, " (goroutine ", detsimWatched.goid, ")")
}

// detsimWatchLock fails the program if gp, the watched goroutine, runs a
// callback and is about to wait for the sync.Mutex or RWMutex at l (see
// detsimSemacquire), for reason, and a goroutine of a node that gp
// paused holds it.
func detsimWatchLock(gp *g, l uintptr, reason waitReason) {
	var hs [8]*g
	n := 0
	systemstack(func() {
		forEachG(func(hg *g) {
			s := detG(hg)
			if n == len(hs) || s.heldGoid != hg.goid || readgstatus(hg)&^_Gscan == _Gdead || detsimState(hg) != detsimPaused {
				return
			}
			for i := 0; i < int(s.nheld); i++ {
				if s.held[i].l == l {
					hs[n] = hg
					n++
					return
				}
			}
		})
	})
	for _, h := range hs[:n] {
		name, paused := detsimWatchNode(detsimNode(h))
		if !paused {
			continue // its node will be resumed by whoever paused it
		}
		kind := "sync.Mutex"
		if reason != waitReasonSyncMutexLock {
			kind = "sync.RWMutex"
		}
		print("detsim: ")
		detsimWatchPrintCall()
		print(" waits for the ", kind, " at ", hex(l), " that goroutine ", h.goid, " of node ", name,
			" holds, and the nemesis paused ", name, ": it would wait forever, as only the nemesis resumes the node\n")
		detsimPrintStack(h)
		fatal("detsim: a nemesis callback waits for a lock that a node the nemesis paused holds; callbacks must not take locks that node goroutines hold")
	}
}

// detsimWatchJump is called by synctestRun with bubble b, whose clock may
// have just jumped. It fails the program once a callback of the watched
// goroutine, in b, has been blocked for detsimWatchSeconds of b's
// time. Only all of b's goroutines blocking moves the clock, so a
// callback started at the time before the first jump it is seen running
// at.
func detsimWatchJump(b *synctestBubble) {
	w := &detsimWatched
	if w.bubble != uintptr(unsafe.Pointer(b)) {
		return
	}
	prev := w.now
	w.now = b.now
	seq := detsimWatchCalls()
	if seq&1 == 0 {
		return
	}
	if seq != w.seq {
		w.seq, w.start = seq, prev
	}
	d := b.now - w.start
	if d < int64(detsimWatchSeconds)*1e9 {
		return
	}
	var gp *g
	systemstack(func() {
		forEachG(func(g1 *g) {
			if g1.goid == w.goid && readgstatus(g1)&^_Gscan != _Gdead {
				gp = g1
			}
		})
	})
	print("detsim: ")
	detsimWatchPrintCall()
	print(" has been blocked for ", d/1e9, "s of the bubble's time\n")
	sep := "detsim: nodes the nemesis paused:"
	for node := 1; node < len(detsim.state); node++ {
		if detsim.state[node] != detsimPaused {
			continue
		}
		if name, paused := detsimWatchNode(int32(node)); paused {
			print(sep, " ", name)
			sep = ","
		}
	}
	if sep == "," {
		print("\n")
	}
	if gp != nil {
		detsimPrintStack(gp)
	}
	fatal("detsim: a nemesis callback has been blocked for too long; callbacks must return promptly, and must not wait for nodes the nemesis paused")
}

// detsimPrintStack prints the stack of gp, which is not running.
func detsimPrintStack(gp *g) {
	stw := stopTheWorld(stwAllGoroutinesStack)
	me := getg()
	systemstack(func() {
		tracebacksomeothers(me, func(g1 *g) bool { return g1 == gp })
	})
	startTheWorld(stw)
}

// detsimSetFailLog registers *p, which the simulation keeps writing to,
// to be written to standard error when the run fails.
func detsimSetFailLog(p *[]byte) { detsim.failLog = p }

// detsimFailLog writes the simulation's failure log if code, the exit
// status, is nonzero, or GODEBUG=dettimeline=1 asks for it, to show what a
// passing run did. Called by detExit and detPanic.
func detsimFailLog(code int) {
	if p := detsim.failLog; p != nil && (code != 0 || debug.dettimeline > 0) {
		// A failed branch's output, with its own timeline, comes first.
		switch {
		case detBranchChild:
			print("sim: fault timeline of branch ", detBranchN, ":\n")
		case detOut.branch != 0:
			print("sim: fault timeline of the timeline (branch ", detOut.branch, "'s is above):\n")
		}
		detWriteAll(2, *p)
	}
}

func detsimSetPanic(f func(node int32, v any)) {
	detsimFatalHook = detsimFatal
	detsim.panic = f
}

// detsimFatalHook is detsimFatal, called indirectly from fatal, which
// some callers reach with write barriers prohibited: the simulation's
// panic handler allocates, but runs only on a node's goroutine, which
// never has them prohibited. detsimSetPanic sets it, as an initializer
// would make an initialization cycle.
var detsimFatalHook func(string)

// detsimFatal offers a fatal error that user code on a node's goroutine
// raised, such as unlocking an unlocked sync.Mutex while a panic unwinds,
// to the simulation, which may crash the node instead, as the error would
// end only the node's process; otherwise it returns and the error ends
// the program. The simulation's own fatal errors still end the program.
func detsimFatal(s string) {
	gp := getg()
	if gp != gp.m.curg || gp.m.locks != 0 || gp.m.mallocing != 0 || len(s) >= 7 && s[:7] == "detsim:" {
		return
	}
	node := detsimNode(gp)
	if node == 0 || detsimState(gp) != detsimLive || gp.bubble == nil {
		return
	}
	// The goroutine's panics, if a defer raised the error, never reach
	// fatalpanic either; see detsimPanic.
	// The message names the panic a defer raised the error in, if any, as
	// the program's crash output would.
	msg := "fatal error: " + s
	n := int32(0)
	for p := gp._panic; p != nil; p = p.link {
		if !p.goexit {
			n++
		}
	}
	if p := gp._panic; p != nil && !p.goexit {
		preprintpanics(p)
		if v, ok := p.arg.(string); ok {
			msg += "\n\twhile panicking: " + v
		}
	}
	runningPanicDefers.Add(-n)
	detsimOffer(gp, node, msg)
	runningPanicDefers.Add(n)
}

// detsimFault offers a fault at addr in a node's goroutine, which sigpanic
// would end the program with, such as a read of memory a node's store
// unmapped, to the simulation, as detsimFatal offers a fatal error: the
// fault would end only the node's process.
func detsimFault(addr uintptr) {
	if detsim.panic == nil {
		return
	}
	var buf [18]byte
	i := len(buf)
	for {
		i--
		buf[i] = "0123456789abcdef"[addr&15]
		if addr >>= 4; addr == 0 {
			break
		}
	}
	i -= 2
	buf[i], buf[i+1] = '0', 'x'
	detsimFatalHook("unexpected fault address " + string(buf[i:]))
}

// detsimPanic offers an unrecovered panic on a node's goroutine to the
// simulation, which may crash the node instead, freezing this goroutine;
// otherwise it returns and the panic ends the program. Called by gopanic
// after preprintpanics, so a value that was an error or a Stringer is
// now its string.
func detsimPanic(p *_panic) {
	gp := getg()
	node := detsimNode(gp)
	if node == 0 || detsimState(gp) != detsimLive || gp.bubble == nil {
		return
	}
	// If the simulation crashes the node, this goroutine never reaches
	// fatalpanic, so it must not count as running panic defers, or a main
	// that returns would wait for it (see main).
	runningPanicDefers.Add(-1)
	detsimOffer(gp, node, p.arg)
	runningPanicDefers.Add(1)
}

// detsimOSExit offers os.Exit on a node's goroutine to the simulation,
// which may crash the node instead, freezing this goroutine, as the exit
// would end only the node's process; otherwise it returns and the exit
// ends the program. Called by os_beforeExit, as the message log.Fatal
// printed is already written.
func detsimOSExit(code int) {
	gp := getg()
	if detsim.panic == nil || gp != gp.m.curg {
		return
	}
	node := detsimNode(gp)
	if node == 0 || detsimState(gp) != detsimLive || gp.bubble == nil {
		return
	}
	var buf [20]byte
	v := uint64(code)
	if code < 0 {
		v = uint64(-code)
	}
	s := string(itoa(buf[:], v))
	if code < 0 {
		s = "-" + s
	}
	detsimOffer(gp, node, "os.Exit("+s+")")
}

// detsimOffer runs the simulation's panic handler for v, raised on gp, the
// running goroutine of node. While the handler runs, the node is paused,
// and gp out of it: the handler may yield, as printing does, and the
// node's other goroutines must not run on the state the panic left half
// updated, such as a mutex unlocked twice. If the handler crashed the
// node, gp leaves its bubble and parks for good; otherwise the node goes
// on and detsimOffer returns.
func detsimOffer(gp *g, node int32, v any) {
	systemstack(func() {
		detsimSetState(node, detsimPaused)
		detG(gp).node = 0
	})
	detsim.panic(node, v)
	if detsim.state[node] == detsimDead {
		systemstack(func() { detsimLeaveBubble(gp, _Grunning) })
		detsimHalt()
	}
	detG(gp).node = node
	detsimResume(node)
}
