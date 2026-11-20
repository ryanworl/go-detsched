// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package runtime

import (
	"internal/runtime/atomic"
	"unsafe"
)

// Fork mode for GOEXPERIMENT=detsched. See the comment above detFork.
// detfork_linux.go and detfork_darwin.go have the system calls.

const (
	detMAP_SHARED = 1
	detECHILD     = 10 // wait4's errno for a child it cannot wait for, on Linux and macOS
)

// Commands from the driver, and replies to it: four little-endian
// uint64s, an operation and its arguments.
const (
	detForkRun    = 1 // run to switch arg1 (0: to the end) and stop there; arg2: detForkRun* flags
	detForkBranch = 2 // fork a child with tape point (here, arg1, arg2); arg3: detForkWith* flags; fds: stdout, stderr, then those flags'
	detForkWait   = 3 // wait for child arg1 to exit
	detForkExit   = 4 // exit now with status 0

	detForkStopped = 1 // stopped at switch arg1; arg2: the new feature + 1 for a stop on novelty, else 0
	detForkPID     = 2 // forked child arg1
	detForkStatus  = 3 // child arg1 exited with wait status arg2
	detForkRefused = 4 // could not fork; arg1 is a detForkReason
	detForkReady   = 5 // a child with its own control socket, at switch arg1
)

// Flags of detForkRun.
const (
	detForkRunStopNovel = 1 // also stop at the switch after the first feature not in the seen map
	detForkRunStopSetup = 2 // also stop at the switch after setup completes (see detSDKSetupComplete)
)

// Flags of detForkBranch, saying which descriptors follow stdout and
// stderr, in this order. Without flags, a third descriptor is a control
// socket.
const (
	detForkWithControl  = 1 // the child's own control socket
	detForkWithFeedback = 2 // where the child writes its feedback at exit (see detFeedReport)
	detForkWithSeen     = 4 // the driver's map of features seen so far, one byte each, mapped read-only
)

// Reasons a fork is refused: a thread the child could not rebuild, or
// shared state it could not separate.
const (
	detForkCgo         = 1 // cgo threads
	detForkNotM0       = 2 // the P is not on the main thread
	detForkBusyM       = 3 // an M that is locked, in a system call, or the template thread
	detForkPollFDs     = 4 // descriptors registered with the network poller
	detForkSameSwitch  = 5 // this process already branched at this switch
	detForkBadFDs      = 6 // the command did not carry the descriptors its flags call for
	detForkCloneFailed = 7
	detForkSecurity    = 8 // the system's certificate verifier was used (darwin)
	detForkFiles       = 9 // regular files are open, or it wrote to the real file system
)

// detForkInit sets s, GODETSEARCH's fork field, as the driver's control
// socket. Called by detSearchInit. The process stops at its first switch.
func detForkInit(s string) {
	n, ok := atoi64u(s)
	if !ok || n == 0 || n > 1<<20 {
		fatal("GODETSEARCH's fork must be a file descriptor")
	}
	detFork.fd = int32(n)
	detFork.stop = 1
}

// detForkServe reports a stop at the current switch and serves commands
// until told to run on. Called by detExecute on the P's thread.
func detForkServe(pp *p) {
	here := detP(pp).switches
	detFork.stop = 0
	novel := uint64(0)
	if detFeed.novelStop {
		novel, detFeed.novelStop = detFeed.novel, false
	}
	if !detForkSend(detForkStopped, here, novel) {
		return
	}
	detForkLoop(pp, here)
}

// detForkLoop serves commands at switch here.
func detForkLoop(pp *p, here uint64) {
	for {
		var cmd [4]uint64
		var fds [5]int32
		nfds, ok := detForkRecv(&cmd, &fds)
		if !ok {
			// The driver is gone: run to the end without stopping.
			closefd(detFork.fd)
			detFork.fd, detFork.stop, detFork.setup = 0, 0, false
			return
		}
		switch cmd[0] {
		case detForkRun:
			detFork.stop = cmd[1]
			if detFork.stop != 0 && detFork.stop <= here {
				detFork.stop = 0
			}
			detFeed.stopNovel = cmd[2]&detForkRunStopNovel != 0
			detFork.setup = cmd[2]&detForkRunStopSetup != 0 && detSetup == 0
			detFeed.novel = 0
			return
		case detForkBranch:
			flags := cmd[3]
			want := 2 + int(flags&1+flags>>1&1+flags>>2&1)
			if flags == 0 && nfds == 3 {
				flags, want = detForkWithControl, 3
			}
			reason := uint64(0)
			switch {
			case nfds != want:
				reason = detForkBadFDs
			case detFork.branched == here:
				reason = detForkSameSwitch
			default:
				reason = detForkCheck()
			}
			if reason != 0 {
				for i := 0; i < nfds; i++ {
					closefd(fds[i])
				}
				detForkSend(detForkRefused, reason, 0)
				continue
			}
			pid, child := detForkClone(pp, detBranchPoint{here, cmd[1], int64(cmd[2])}, flags, fds[:nfds])
			if child {
				if detFork.fd == 0 {
					return
				}
				// A child with its own control socket stops here too.
				if !detForkSend(detForkReady, here, 0) {
					return
				}
				continue
			}
			for i := 0; i < nfds; i++ {
				closefd(fds[i])
			}
			if pid < 0 {
				detForkSend(detForkRefused, detForkCloneFailed, uint64(-pid))
			} else {
				detForkSend(detForkPID, uint64(pid), 0)
			}
		case detForkWait:
			var status int32
			r, e := detForkWait4(int64(cmd[1]), &status, 0)
			if e != 0 {
				detForkSend(detForkStatus, cmd[1], 1<<63|uint64(e))
			} else {
				detForkSend(detForkStatus, uint64(r), uint64(uint32(status)))
			}
		case detForkExit:
			exit(0)
		}
	}
}

// detForkCheck reports why the process cannot fork now, or 0. Every M
// other than this one must be idle or the watchdog, because a child has
// only the thread that forked and must start the others again.
func detForkCheck() uint64 {
	if iscgo {
		return detForkCgo
	}
	if getg().m != &m0 {
		return detForkNotM0
	}
	if detFork.pollOpen != 0 {
		return detForkPollFDs
	}
	if detFork.security {
		return detForkSecurity
	}
	if atomic.Loadint32(&detFork.files) != 0 || atomic.Loadint32(&detFork.wroteFS) != 0 {
		return detForkFiles
	}
	// An M that just gave up the P, such as one that handed it to a
	// locked goroutine, is on its way to park; give it time to.
	for try := 0; !detForkMsIdle(); try++ {
		if try == 1000 {
			return detForkBusyM
		}
		usleep(50)
	}
	return 0
}

// detForkWhy describes a reason detForkCheck gives.
func detForkWhy(r uint64) string {
	switch r {
	case detForkCgo:
		return "cgo"
	case detForkNotM0:
		return "the P is not on the main thread"
	case detForkBusyM:
		return "an M is locked to a goroutine, in a system call, or slow to park"
	case detForkPollFDs:
		return "descriptors are registered with the network poller"
	case detForkSecurity:
		return "the system's certificate verifier was used"
	case detForkFiles:
		return "it uses the real file system, which a fork would share with this process"
	}
	return "unknown reason"
}

// detForkMsIdle reports whether every M other than m0 and the watchdog is
// parked idle.
func detForkMsIdle() bool {
	lock(&sched.lock)
	defer unlock(&sched.lock)
	for mp := allm; mp != nil; mp = mp.alllink {
		if mp == &m0 || mp == detFork.watchdog.ptr() {
			continue
		}
		if sched.midle.head() != unsafe.Pointer(mp) && mp.idleNode.prev == 0 && mp.idleNode.next == 0 {
			return false
		}
	}
	return true
}

// detForkClone forks the process. In the parent it returns the child's
// PID, or minus the errno. In the child it returns child true, after
// making the child consistent: the other Ms get new threads, the network
// poller a new epoll or kqueue instance, fds 1 and 2 the driver's descriptors, and
// the tape the branch point b. The descriptors are stdout, stderr, and
// those flags call for.
func detForkClone(pp *p, b detBranchPoint, flags uint64, fds []int32) (pid int64, child bool) {
	pid, child = detForkProcess()
	if !child {
		return pid, false
	}
	detForkDup2(fds[0], 1)
	detForkDup2(fds[1], 2)
	closefd(fds[0])
	closefd(fds[1])
	closefd(detFork.fd)
	detFork.fd, detFork.stop, detFork.setup, detFork.branched = 0, 0, false, b.switchN
	fds = fds[2:]
	if flags&detForkWithControl != 0 {
		detFork.fd, fds = fds[0], fds[1:]
	}
	if flags&detForkWithFeedback != 0 {
		// Count only what the child does after the branch.
		detFeed.on, detFeed.out, fds = true, fds[0], fds[1:]
		detFeed.hits = [len(detFeed.hits)]uint8{}
	}
	detFeed.seen, detFeed.novel, detFeed.novelSwitch, detFeed.novelStop, detFeed.stopNovel = nil, 0, 0, false, false
	if flags&detForkWithSeen != 0 {
		p, err := mmap(nil, uintptr(len(detFeed.hits)), _PROT_READ, detMAP_SHARED, fds[0], 0)
		if err == 0 {
			detFeed.seen = (*[len(detFeed.hits)]uint8)(p)
		}
		closefd(fds[0])
	}

	// The child's tape is the parent's up to here, then b.
	tape := unsafe.Slice((*detBranchPoint)(persistentalloc(uintptr(detTapeNext+1)*unsafe.Sizeof(detBranchPoint{}), 8, &memstats.other_sys)), detTapeNext+1)
	copy(tape, detTape[:detTapeNext])
	tape[detTapeNext] = b
	detTape = tape
	return 0, true
}

// detForkProcess forks the process. In the parent it returns the child's
// PID, or minus the errno. In the child it returns child true, after
// giving the other Ms new threads and the network poller a new epoll or
// kqueue instance.
func detForkProcess() (pid int64, child bool) {
	lock(&detFork.lock) // the watchdog is not in the middle of a check
	lock(&sched.lock)   // no M changes state
	var oset sigset
	sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
	r, e := detForkFork()
	if e != 0 || r != 0 {
		sigprocmask(_SIG_SETMASK, &oset, nil)
		unlock(&sched.lock)
		unlock(&detFork.lock)
		if e != 0 {
			return -int64(e), false
		}
		return int64(r), false
	}

	// In the child, the only thread. The parent's other threads may have
	// been waiting for a lock, such as the watchdog for detFork.lock, and
	// their state came along: the lock lists their Ms as waiters, and each
	// M's semaphore counts the wait. When such a thread starts again below
	// and waits once more, unlock would walk a list with its M in it twice,
	// or its wakeup would only cancel the old count. Keep the locks held,
	// without waiters, and start the Ms with their semaphores clear
	// (detForkResetM).
	detForkSelf()
	sched.lock.key = mutexLocked
	detFork.lock.key = mutexLocked
	for mp := allm; mp != nil; mp = mp.alllink {
		detForkResetM(mp)
		switch mp {
		case &m0:
		case detFork.watchdog.ptr():
			mp.mstartfn = detWatchdogLoop
			newosproc(mp)
		default: // idle, parked in stopm
			mp.mstartfn = detForkRepark
			newosproc(mp)
		}
	}
	unlock(&sched.lock)
	if netpollinited() {
		detForkPollReinit()
	}
	sigprocmask(_SIG_SETMASK, &oset, nil)
	unlock(&detFork.lock)
	if f := detsim.forked; f != nil {
		// The ticks f counts do not move the tick clock: a branch run
		// in place (GODETBRANCH) does not run f, and GODETSTOP=kN on
		// its replay line must stop where the forked child was at
		// tick N.
		now := detTickNow()
		ticks, q, last := detsimEnter()
		systemstack(func() { f() })
		detsimExit(ticks, q, last)
		detTickEnd -= detTickNow() - now
		detPutTicks(detTicksReal())
	}
	return 0, true
}

// Branches that end a timeline.
//
// A test composer runs an eventually or finally check on a branch of the
// timeline, as Antithesis does, so that the timeline goes on: the child
// runs the check and exits, and the parent waits for it, then continues as
// if it had not forked. Only the goroutine holding the P runs while the
// parent waits, so its schedule does not change. A branch's output shows
// only if it fails: the branch writes it to memory it shares with the
// parent, which puts it at the head of its own buffered output, so that a
// driver sees the branch's failure first. A process the driver runs to a
// stop, to fork runs from it there, forks no branches.
// A branch reports only the assertions that fail in it, not those the
// timeline failed before it forked. When the timeline had failed already,
// its failure came first, so the branch's output goes after the
// timeline's report instead (detOut.late), as a symptom of it, and the
// timeline forks no more branches, which would show the same.

// detsimBranch forks a branch that ends the timeline. In the child it
// returns the branch's number, from 1, which GODETBRANCH takes to run
// the branch in place. In the parent it waits for the child and returns
// 0; a child that exits with a nonzero status, or that wait4 fails for,
// records the assertion failure msg in the parent, with the status or
// wait4's errno. It returns 0 without forking when the process cannot
// fork now, is a branch itself, or a branch failed after it had. Package
// sim calls it (see detsim_sdk.go).
func detsimBranch(msg string) int {
	if detBranchChild || detOut.late.head != nil {
		return 0
	}
	// Number the branch before asking whether the process can fork: the
	// answer can depend on real time, such as how soon another M parks or
	// whether detWatchdog moved the P off the main thread, so a branch that
	// did not run must not renumber the ones after it, or GODETBRANCH would
	// replay another branch than the one that failed.
	detBranchN++
	if n, ok := atoi64u(gogetenv("GODETBRANCH")); ok {
		// Run branch n in this process, as its child would run, and pass
		// over the others as if they had passed, so that a debugger,
		// which does not follow a fork, can replay a failing branch.
		// GODETBRANCH=0 runs the timeline without any branch.
		if uint64(detBranchN) != n {
			return 0
		}
		detBranchChild = true
		detSDKBranch()
		return detBranchN
	}
	if detFork.fd != 0 && (detFork.stop != 0 || detFeed.stopNovel || detFork.setup) {
		// The driver runs this process to a stop, to fork runs from it
		// there: a search's setup root, or the program of a kept cell.
		// Its output goes nowhere, and a branch's failure here would be
		// copied into every run forked from it, whose replay lines could
		// not reproduce it: a replay runs the branch in place, where the
		// run's later branch points apply inside it. The runs fork the
		// branches they reach themselves, as the plain runs do.
		return 0
	}
	if r := detForkCheck(); r != 0 {
		print("detsched: branch ", detBranchN, " did not run: the process cannot fork now (", detForkWhy(r), ")\n")
		return 0
	}
	var share *detShare
	if p, err := mmap(nil, detShareSize, _PROT_READ|_PROT_WRITE, _MAP_ANON|detMAP_SHARED, -1, 0); err == 0 {
		share = (*detShare)(p)
	}
	pid, child := detForkProcess()
	if child {
		detBranchChild = true
		detSDKBranch()
		if detFork.fd != 0 {
			closefd(detFork.fd) // the driver's, as are the feedback descriptor and the seen map
		}
		detFork.fd, detFork.stop, detFork.setup = 0, 0, false
		if detFeed.out > 0 {
			closefd(detFeed.out)
		}
		detFeed.out, detFeed.seen = 0, nil
		detOut.head, detOut.tail = nil, nil // the parent writes what came before
		detOut.onFailure = true
		detOut.share = share // without it, the branch writes its output itself
		return detBranchN
	}
	if share != nil {
		defer munmap(unsafe.Pointer(share), detShareSize)
	}
	if pid < 0 {
		// The system refused, such as for the limit on processes: say so,
		// as for a refusal above, rather than pass over the branch.
		print("detsched: branch ", detBranchN, " did not run: the process cannot fork now (fork failed: ")
		if -pid == _EAGAIN {
			print("EAGAIN)\n")
		} else {
			print("errno ", -pid, ")\n")
		}
		return 0
	}
	var status int32
	var e uintptr
	for {
		_, e = detForkWait4(pid, &status, 0)
		if e != _EINTR {
			break
		}
	}
	if e != 0 || status != 0 {
		// Build the details on the stack: a heap allocation here, which a
		// replay with GODETBRANCH does not make, would move every later
		// allocation, and with it the schedule of a program whose work
		// depends on heap addresses.
		var buf, nbuf [20]byte
		what := " exited with status "
		code := itoa(buf[:], uint64(status>>8)&0xff)
		if sig := status & 0x7f; sig != 0 {
			what, code = " died of signal ", itoa(buf[:], uint64(sig))
		}
		if e != 0 {
			// The child's status is unknown, and so is whether it is
			// done writing its output: report the wait's error rather
			// than a status the child may never have had.
			what, code = ": wait4 failed: errno ", itoa(buf[:], uint64(e))
			if e == detECHILD {
				what, code = ": wait4 failed: ", buf[:copy(buf[:], "ECHILD")]
			}
		} else if share != nil {
			detOutTakeBranch(share, detBranchN, detSDK.failures > 0)
		}
		n := itoa(nbuf[:], uint64(detBranchN))
		var d [96]byte
		k := copy(d[:], "branch ")
		k += copy(d[k:], n)
		k += copy(d[k:], what)
		k += copy(d[k:], code)
		k += copy(d[k:], " (GODETBRANCH=")
		k += copy(d[k:], n)
		k += copy(d[k:], " runs it in place)")
		detSDKAssert(detAssertAlways, false, msg, unsafe.String(&d[0], k))
	}
	return 0
}

// detForkRepark is the start function of an idle M's new thread in a
// child: it parks again, as the M's old thread was in stopm, and mstart1
// then takes the P it is given, as stopm would.
func detForkRepark() {
	mPark()
}

// detForkSend sends a reply to the driver.
func detForkSend(op, a1, a2 uint64) bool {
	msg := [4]uint64{op, a1, a2, 0}
	return write1(uintptr(detFork.fd), unsafe.Pointer(&msg), int32(unsafe.Sizeof(msg))) == int32(unsafe.Sizeof(msg))
}
