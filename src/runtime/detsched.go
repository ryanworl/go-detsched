// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Support for GOEXPERIMENT=detsched, which makes single-threaded programs
// that use testing/synctest and do not interact with the OS run
// deterministically.
//
// GODETSEED (see below) also says that the program runs as one of many
// seeds to explore, or to replay one: with it set, the runtime perturbs
// the schedule (see detPerturb) and reports. Without it, the program runs
// deterministically with seed 0, its schedule unperturbed, and reports
// nothing unless GODEBUG=dettrace asks for it.
//
// At exit, the runtime prints a hash of the program's schedule to standard
// error:
//
//	detsched: trace=0x... switches=N syncs=N
//
// Each switch to a goroutine folds that goroutine's ID and the code
// offset it resumes at into the hash, so two runs print the same line
// only if they switched between the same goroutines at the same points.
// The hash is only meaningful with a single P.
//
// GODEBUG=dettrace=1 prints it without GODETSEED, and dettrace=2 also
// prints every switch as it happens, so the logs of two runs can be diffed
// to find where they diverge.
//
// The environment variable GODETSEARCH is an internal interface for the
// detsched command, which drives the search with it; see detSearchInit.
//
// All of the runtime's randomness, which picks map seeds and iteration
// order, select order, hash keys, the heap base address, and the
// math/rand and math/rand/v2 global generators, comes from one seed.
// In det mode that seed is GODETSEED, a decimal uint64 that defaults to 0,
// instead of random data from the OS. GODETSEED is read only on systems
// where getenvEarly works; elsewhere the seed is always 0.
//
// On Unix, writes to file descriptors 1 and 2 through package syscall
// (which includes os.Stdout, os.Stderr, fmt, and log) go to an in-memory
// buffer instead, so a goroutine never blocks in a write that the OS may
// finish late. The buffer is written to the real fds at exit, including
// fatal panics and throws. The runtime's own output, such as panic
// messages and println, is written directly.
//
// Goroutines are preempted after a fixed amount of work instead of a
// fixed amount of time. The compiler counts a tick at the entry of every
// function and on every loop backedge, outside the runtime and the
// packages it imports and outside os, syscall, and internal/poll (see
// ssagen.detPreempt), by decrementing detTicks (see ssagen's detTick and
// ssa.insertLoopReschedChecks). When detTicks reaches zero it calls
// detPreempt. Without perturbation, execute gives each goroutine
// detQuantum ticks when it starts running. The compiler also emits a
// synchronization point before every channel operation, select, go
// statement, and atomic operation, which decrements detSyncs and calls
// detSync when it reaches zero; only scheduling perturbation uses them.
//
// synctest bubbles never wait in real time. Upstream, a bubble's clock
// advances only when all its goroutines are durably blocked, so a goroutine
// blocked on a sync.Mutex, an RWMutex, or a channel created outside the
// bubble keeps the bubble's timers from firing forever. In det mode, when
// the scheduler finds no goroutine anywhere that can run, and none is in a
// system call or waiting on the network poller, nothing outside the
// bubbles can unblock them either, so their blocked goroutines count as
// durably blocked: a pending synctest.Wait returns, or else the bubble's
// clock jumps to its next timer, or else synctest.Test reports a deadlock.
// The rule acts on one bubble at a time: first one with a pending Wait or a
// timer already due, then the one whose next timer is earliest, then the
// oldest, which deadlocks (see detSynctestIdle). Each clock jump is also
// folded into the schedule hash.
//
// The runtime also reports where a program leaves the deterministic
// subset, by depending on the OS or on real time:
//
//   - a goroutine waits for a file or network connection to become ready
//     (netpollblock);
//   - a goroutine stays in a system call long enough that detWatchdog
//     hands off its P (see below);
//   - a timer outside every synctest bubble fires, so real time decided
//     when something happened.
//
// Each report includes the goroutine's stack, each distinct stack is
// reported once, and the trace report at exit counts them.
//
// There is no sysmon, so nothing takes the P away from a goroutine in a
// system call, and the program would hang whenever the call waits for
// something another goroutine must do first: os/exec waiting for a child
// that writes to a pipe the program reads, or a blocking read from a
// terminal another goroutine writes to. The detWatchdog thread does only
// that part of sysmon's job: once a goroutine has been in the same system
// call for at least detSyscallRetakeNS, it hands off the P so other
// goroutines can run. Short system calls keep the P, so they leave the
// schedule deterministic; a hand-off does not, and is reported.

package runtime

import (
	"internal/abi"
	"internal/bytealg"
	"internal/byteorder"
	"internal/chacha8rand"
	"internal/goarch"
	"internal/goexperiment"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"internal/stringslite"
	"math/bits"
	"unsafe"
)

// detPState is the det-mode state of a P. p.det has length 1 under the
// experiment and 0 otherwise; see detP.
type detPState struct {
	trace    uint64       // hash of goroutine switches and bubble clock jumps
	switches uint64       // number of goroutine switches
	rand     detRandState // random state of the M holding this P

	// sched is the random state behind scheduling perturbation. It is
	// separate from rand, so turning perturbation on or tuning it does
	// not change the random values the program sees.
	sched chacha8rand.State

	// fault is the random state behind simulated faults; see detsim.go.
	fault chacha8rand.State
}

// detGState is the det-mode state of a goroutine, used by PCT. g.det has
// length 1 under the experiment and 0 otherwise; see detG.
type detGState struct {
	goid uint64 // the goroutine the state belongs to: g structs are reused
	prio int64  // priority; the highest-priority runnable goroutine runs
	used int32  // ticks run at this priority
	node int32  // node handle in a simulation; see detsim.go

	// spawn, if not 0, is one more than the node handle of the goroutines
	// this goroutine starts; see detsimSetNode.
	spawn int32

	busy detBusyState // see detbusy.go

	// wait is the time the simulation's handling of the goroutine's
	// system call makes it wait once the call returns, and waiting is set
	// while it does; see detsimWait.
	wait    int64
	waiting bool

	// bubble is the goroutine's bubble while gcStart or gcAssistAlloc
	// sets it aside, and aside is set meanwhile; see detsimSetAside.
	bubble *synctestBubble
	aside  bool

	// For a goroutine of a crashed node: frozen is set once
	// detsimFreeze parked it for good, and zombie is the address of a
	// sync.Mutex it holds that a live goroutine waits for, which it
	// runs on to release; see detsimSemacquire.
	frozen bool
	zombie uintptr
	left   *synctestBubble // the bubble it left on the crash

	// woke is the sync.Mutex whose semaphore woke the goroutine last,
	// until it takes the mutex; see detsimSemrelease.
	woke uintptr

	// Mutexes the goroutine holds, for feedback; see detfeed.go.
	heldGoid uint64 // the goroutine held belongs to
	nheld    int32
	held     [4]detHeldLock
}

// detG returns gp's det-mode state.
func detG(gp *g) *detGState {
	return (*detGState)(unsafe.Pointer(&gp.det))
}

// detP returns pp's det-mode state.
func detP(pp *p) *detPState {
	return (*detPState)(unsafe.Pointer(&pp.det))
}

// detRandState is the random state behind rand and cheaprand while an M
// holds a P. It lives on the P so that the random values a goroutine sees
// do not depend on which M runs it.
type detRandState struct {
	chacha8     chacha8rand.State
	cheaprand   uint32
	cheaprand64 uint64
}

// detMaxSwitches is GODETSEARCH's maxswitches: a run fails when it makes more
// switches since its last branch (detBranchSwitch), so that a program
// looping in virtual time ends, and a run branched from deep in a guided
// search is not cut short by the switches of the runs before it.
var detMaxSwitches uint64

// detExecute is called by execute when gp is about to run on pp.
func detExecute(pp *p, gp *g, inheritTime bool) {
	detTraceSwitch(pp, gp)
	if detMaxSwitches != 0 && detP(pp).switches-detBranchSwitch > detMaxSwitches {
		throw("detsched: the run made more switches than maxswitches")
	}
	if detFeed.on {
		detFeedMaxOf(detFeedMaxGoroutines, uint64(gcount(false)))
		if detCover.n > 0 && detP(pp).switches%detCoverEvery == 0 {
			detFeedCoverScan()
		}
	}
	// A stop waits until the P is on the main thread, which a fork needs
	// (see detForkCheck). It is elsewhere while a locked goroutine waits,
	// such as the main goroutine during the runtime's init.
	if detFork.stop != 0 && detP(pp).switches >= detFork.stop && getg().m == &m0 {
		detForkServe(pp)
	}
	if detTapeNext < len(detTape) && detP(pp).switches == detTape[detTapeNext].switchN {
		detBranch(pp)
	}
	if detBusy.on {
		detBusyCount() // the last goroutine's ticks
		detBusy.last = nil
	}
	switch {
	case inheritTime:
	case detPerturb.pct:
		detPCTExecute(pp, gp)
	case detPerturb.quantum > 0:
		detPutTicks(detExpRand(pp, detPerturb.quantum))
	default:
		detPutTicks(detQuantum)
	}
	if detBusy.on {
		detBusyClamp(gp)
	}
	if detStops.on {
		detStopsCheck(pp, gp)
	}
}

// detQuantum is the number of ticks a goroutine runs before it is
// preempted, without perturbation, and PCT's budget of ticks at one
// priority. GODETSEARCH's quantum field sets it, for tests that need
// preemption at every tick.
var detQuantum int32 = 100000

// detExit is called by runExitHooks.
func detExit(code int) {
	if code == 0 && detSDK.failures > detSDK.base {
		code = 3
	}
	detOutputFlush(code)
	if detOut.onFailure && code == 0 {
		return // a forked branch that passed is silent; see detsimBranch
	}
	detsimFailLog(code)
	detTraceReport()
	detFeedReport(code)
	detSDKReport()
	detOut.late.write()
	if code == 3 {
		exit(3)
	}
}

// detPanic is called by startpanic_m, as the program dies of a panic or a
// fatal error without running the exit hooks.
func detPanic() {
	if !goexperiment.DetSched {
		return
	}
	detOutputFlush(2)
	detsimFailLog(2)
	detTraceReport()
	detFeedReport(2)
	detSDKReport()
	detOut.late.write()
}

// detTraceSwitch records in pp's schedule hash that gp is about to run.
func detTraceSwitch(pp *p, gp *g) {
	pc := gp.sched.pc
	if pc >= firstmoduledata.text && pc < firstmoduledata.etext {
		pc -= firstmoduledata.text // independent of where the text is loaded
	}
	detP(pp).trace = detMix(detMix(detP(pp).trace, gp.goid), uint64(pc))
	detP(pp).switches++
	if debug.dettrace >= 2 {
		f := findfunc(gp.sched.pc)
		print("detsched: switch ", detP(pp).switches, " to goroutine ", gp.goid, " at ")
		if f.valid() {
			print(funcname(f), "+", hex(gp.sched.pc-f.entry()), "\n")
		} else {
			print(hex(gp.sched.pc), "\n")
		}
	}
}

// detTicks is the number of ticks the running goroutine has left before
// it is preempted, or before a stop when detTickExtra holds back the rest
// (see detstop.go). Compiled code decrements it; see the comment at the
// top of this file. Only the goroutine holding the single P touches it.
// The runtime sets it only through detPutTicks, and reads the goroutine's
// budget as detTicksReal.
var detTicks int32

// detPreempt is called by compiled code when detTicks runs out. It yields
// the P, like goschedguarded, unless preemption is not allowed here, in
// which case the goroutine tries again at its next tick.
//
//go:nosplit
func detPreempt() {
	if detDebug.nwatch > 0 && detWatchEntry(sys.GetCallerPC()) && detTicks > 0 {
		return
	}
	gp := getg()
	if detTicks <= 0 && detTickNow() >= detStops.next {
		// A stop is due (see detstop.go). On a system stack it waits
		// for the next tick, keeping the goroutine's budget. A call
		// from detSync, with ticks left, yields as it would have.
		if gp != gp.m.g0 && gp != gp.m.gsignal {
			detStopFire(gp, sys.GetCallerPC(), sys.GetCallerSP())
		} else if detTickExtra > 0 {
			detPutTicks(detTicksReal())
		}
		if detTicks > 0 {
			return
		}
	}
	if gp == gp.m.g0 || gp == gp.m.gsignal {
		detSetTicks(1)
		return
	}
	if detBusy.on && detBusyPay() {
		return
	}
	mcall(detPreempt_m)
}

func detPreempt_m(gp *g) {
	if !canPreemptM(gp.m) {
		detSetTicks(1)
		gogo(&gp.sched) // never return
	}
	if detPerturb.pct {
		detPCTPreempt(gp)
	}
	goschedImpl(gp, true)
}

// detSeed returns the value of GODETSEED, and sets detSeedSet. Called by
// randinit, before mallocinit, so it must not allocate.
func detSeed() uint64 {
	s, _ := getenvEarly("GODETSEED")
	if s == "" {
		return 0
	}
	n, ok := atoi64u(s)
	if !ok {
		fatal("GODETSEED must be a decimal uint64")
	}
	detSeedSet = true
	return n
}

// detSeedValue is GODETSEED, set by randinit, and detSeedSet reports
// whether GODETSEED is set, which turns perturbation and reports on.
var (
	detSeedValue uint64
	detSeedSet   bool
)

// detReporting reports whether the runtime prints its reports: the trace
// at exit and where the program leaves the deterministic subset.
func detReporting() bool {
	return detSeedSet || debug.dettrace > 0
}

// detSeedGlobalRand replaces the OS-provided seed of the global random
// generator with GODETSEED. Called by randinit, before mallocinit. On
// macOS, it first runs the program again without ASLR if needed
// (detNoASLR).
func detSeedGlobalRand(seed *[32]byte) {
	detNoASLR()
	detSeedValue = detSeed()
	clear(seed[:])
	byteorder.LEPutUint64(seed[:], detSeedValue)
}

// getenvEarly is like getGodebugEarly for any environment variable.
// It does not allocate, so it can run before mallocinit.
func getenvEarly(key string) (string, bool) {
	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}
		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := unsafe.String(p, findnull(p))
			if len(s) > len(key) && s[len(key)] == '=' && stringslite.HasPrefix(s, key) {
				return gostringnocopy(p)[len(key)+1:], true
			}
		}
		return "", true
	}
	return "", false
}

// detRand, detCheaprand, and detCheaprandu64 replace rand, cheaprand, and
// cheaprandu64 while the M holds a P, drawing from the P's state.

//go:nosplit
func detRand() uint64 {
	mp := getg().m
	c := &detP(mp.p.ptr()).rand.chacha8
	for {
		x, ok := c.Next()
		if ok {
			return x
		}
		mp.locks++ // keep the P while c.Refill may do stack split checks
		c.Refill()
		mp.locks--
	}
}

// mcheaprand, mcheaprandn, and mcheaprandu64 are cheaprand, cheaprandn,
// and cheaprandu64 for runtime code that draws a number of times that
// depends on which M runs it or on contention between threads, such as
// evicting from a per-M cache or sampling runtime lock contention. In det
// mode they draw from the M's own random state, so those draws don't shift
// the P's, which decides what the program sees.
//
//go:nosplit
func mcheaprand() uint32 {
	if !goexperiment.DetSched {
		return cheaprand()
	}
	mp := getg().m
	mp.cheaprand += 0x53c5ca59
	hi, lo := bits.Mul32(mp.cheaprand, mp.cheaprand^0x74743c1b)
	return hi ^ lo
}

//go:nosplit
func mcheaprandn(n uint32) uint32 {
	return uint32((uint64(mcheaprand()) * uint64(n)) >> 32)
}

//go:nosplit
func mcheaprandu64() uint64 {
	return uint64(mcheaprand())<<32 | uint64(mcheaprand())
}

//go:nosplit
func detCheaprand() uint32 {
	r := &detP(getg().m.p.ptr()).rand
	r.cheaprand += 0x53c5ca59
	hi, lo := bits.Mul32(r.cheaprand, r.cheaprand^0x74743c1b)
	return hi ^ lo
}

//go:nosplit
func detCheaprandu64() uint64 {
	r := &detP(getg().m.p.ptr()).rand
	r.cheaprand64 += 0xa0761d6478bd642f
	hi, lo := bits.Mul64(r.cheaprand64, r.cheaprand64^0xe7037ed1a0b428db)
	return hi ^ lo
}

// detMutexClock replaces wall-clock time in sync.Mutex's decision to
// enter starvation mode (see internal_sync_nanotime). It counts goroutine
// switches as 250µs each, so a waiter starts starvation mode once more
// than 4 switches have happened while it waited for the mutex.
func detMutexClock() int64 {
	if pp := getg().m.p.ptr(); pp != nil {
		return int64(detP(pp).switches) * 250 * 1000
	}
	return nanotime()
}

// detStartWatchdog starts detWatchdog. Called by main.
func detStartWatchdog() {
	systemstack(func() {
		newm(detWatchdog, nil, -1)
	})
}

// crypto_internal_rand_detSeed returns GODETSEED to crypto/internal/rand,
// which seeds all cryptographic randomness from it in det mode.
//
//go:linkname crypto_internal_rand_detSeed crypto/internal/rand.detSeed
func crypto_internal_rand_detSeed() uint64 {
	return detSeedValue
}

// detRandInit seeds pp's random state from GODETSEED and pp's ID, so it
// does not depend on how much of the global random stream was used before.
// Called by (*p).init.
func detRandInit(pp *p) {
	detSeedStreams(pp, detSeedValue, uint64(pp.id))
	if pp.id == 0 {
		// The watchdog holds detFork.lock as sysmon holds sysmonlock, and
		// it replaces sysmon, so the lock takes sysmon's lock rank.
		lockInit(&detFork.lock, lockRankSysmon)
		if !detSeedSet {
			detPerturbStrategy = 0
		}
		detSearchInit()
		detStopInit()
		detPerturbInit(pp, 0, 0)
	}
}

// detSearchInit parses GODETSEARCH, the internal interface through which
// the detsched command drives a search. It is not for users. It is a list
// of space-separated fields:
//
//	tape=N:S[:K],...      branch points (see detTapeInit)
//	fork=FD               serve a driver over socket FD (see detForkInit)
//	feedback=1|PATH       record features (see detFeedInit)
//	cover=1|counts        record coverage features (see detFeedCoverInit)
//	yield=0xA,...         hold locks acquired at these sites (see detFeedYieldInit)
//	maxswitches=N         fail after N switches since the last branch
//	perturb=off|walk|pct  use one strategy instead of drawing one per run
//	quantum=N             ticks per quantum without perturbation (see detQuantum)
//	trace=0xH             the schedule hash the run should have (see detExpectTrace)
//
// Replaying a run needs the fields that change what it does: all but
// fork, feedback, cover, and trace, which a replay line adds to check the
// replay.
func detSearchInit() {
	s := gogetenv("GODETSEARCH")
	for s != "" {
		f := s
		if i := bytealg.IndexByteString(s, ' '); i >= 0 {
			f, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		k, v, _ := stringslite.Cut(f, "=")
		ok := true
		switch k {
		case "":
		case "tape":
			detTapeInit(v)
		case "fork":
			detForkInit(v)
		case "feedback":
			detFeedInit(v)
		case "cover":
			detFeedCoverInit(v)
		case "yield":
			detFeedYieldInit(v)
		case "maxswitches":
			detMaxSwitches, ok = atoi64u(v)
		case "trace":
			detExpectTrace, ok = detParseHex(v)
		case "envread":
			detEnvRead.report = true
		case "quantum":
			var n uint64
			n, ok = atoi64u(v)
			ok = ok && n > 0 && n < 1<<31
			detQuantum = int32(n)
		case "perturb":
			switch v {
			case "off":
				detPerturbStrategy = 0
			case "walk":
				detPerturbStrategy = 1
			case "pct":
				detPerturbStrategy = 2
			default:
				ok = false
			}
		default:
			ok = false
		}
		if !ok {
			print("detsched: GODETSEARCH field ", f, "\n")
			fatal("GODETSEARCH has an unknown field or a bad value")
		}
	}
}

// detExpectTrace is the schedule hash of the run a replay line replays,
// or 0. A replay that runs another schedule says so at exit, as it may not
// do what the run did, which only its trace line shows otherwise.
var detExpectTrace uint64

// detParseHex parses a hexadecimal number with a 0x prefix.
func detParseHex(s string) (uint64, bool) {
	s, ok := stringslite.CutPrefix(s, "0x")
	if !ok || s == "" || len(s) > 16 {
		return 0, false
	}
	var h uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		default:
			return 0, false
		}
		h = h<<4 | uint64(c)
	}
	return h, true
}

// detSeedStreams seeds pp's random state and sched stream from the key
// (a, b): GODETSEED and the P's ID at startup, or a branch's seed and
// switch number (see detBranch).
func detSeedStreams(pp *p, a, b uint64) {
	r := &detP(pp).rand
	r.chacha8.Init64([4]uint64{a, b, 0x6465747363686564, 0})
	next := func() uint64 {
		for {
			if x, ok := r.chacha8.Next(); ok {
				return x
			}
			r.chacha8.Refill()
		}
	}
	r.cheaprand = uint32(next())
	r.cheaprand64 = next()
	detP(pp).sched.Init64([4]uint64{a, b, 0x6465747363686564, 1})
	detsimSeed(pp, a, b)
}

// detStartlockedm wakes mp, which now owns this M's former P, and stops
// this M, as startlockedm does, but puts this M on the idle list first.
// Otherwise mp may call startm before this M is idle, and whether startm
// reuses an idle M or allocates a new one from the P's heap cache would
// depend on how the threads race, shifting heap addresses and the heap
// profiler's random draws.
func detStartlockedm(mp *m) {
	gp := getg()
	lock(&sched.lock)
	mput(gp.m)
	unlock(&sched.lock)
	notewakeup(&mp.park)
	mPark()
	acquirep(gp.m.nextp.ptr())
	gp.m.nextp = 0
}

// detMix folds v into the hash h. It must not depend on any per-process
// seed, unlike the runtime's memhash.
func detMix(h, v uint64) uint64 {
	h = (h ^ v) * 0x9e3779b97f4a7c15
	return h ^ h>>29
}

// detTraceReport prints the schedule hash if the runtime reports (see
// detReporting).
func detTraceReport() {
	if !goexperiment.DetSched || !detReporting() {
		return
	}
	var h, n uint64
	// A fatal error can happen with allpLock held; the world is frozen then.
	locked := panicking.Load() == 0
	if locked {
		lock(&allpLock)
	}
	for _, pp := range allp {
		if detP(pp).switches == 0 {
			continue
		}
		h = detMix(detMix(h, detP(pp).trace), detP(pp).switches)
		n += detP(pp).switches
	}
	if locked {
		unlock(&allpLock)
	}
	print("detsched: trace=", hex(h), " switches=", n, " syncs=", detSyncStep(), "\n")
	if detExpectTrace != 0 && h != detExpectTrace {
		print("detsched: warning: this run's schedule is not the one its replay line was printed for (trace=", hex(detExpectTrace), "), so it may not do what that run did. Its environment, the files it reads, or the binary differ from the search's: a variable set here that the search's environment did not have, such as NO_COLOR, can change the schedule; unset it with env -u NAME\n")
	}
	if detBranchChild {
		print("detsched: branch ", detBranchN, " (GODETBRANCH=", detBranchN, " runs it in place)\n")
	}
	if detEnvRead.report && detEnvRead.n > 0 {
		print("detsched: environment read: ")
		gwrite(detEnvRead.buf[:detEnvRead.n])
		print("\n")
	}
	if len(detTape) > 0 {
		print("detsched: tape")
		for i, b := range detTape {
			if i == detTapeNext {
				print(" (not reached:")
			}
			print(" ", b.switchN, ":", b.seed)
			if b.steps > 0 {
				print(":", b.steps)
			}
		}
		if detTapeNext < len(detTape) {
			print(")")
		}
		print("\n")
	}
	if detSetup != 0 {
		print("detsched: setup complete at switch ", detSetup, "\n")
	}
	if detPerturb.pct {
		print("detsched: perturb pct depth=", detPCT.depth, " steps=", detPCT.steps, "\n")
	} else if detPerturb.quantum > 0 {
		print("detsched: perturb walk quantum=", detPerturb.quantum, " pick=", detPerturb.pick, "/16 wake=", detPerturb.wake, "/16 sync=", detPerturb.sync, "/16\n")
	}
	detBusyReport()
	if detStrictSeen.reports > 0 {
		print("detsched: the program left the deterministic subset; ", detStrictSeen.reports, " distinct reports above\n")
	}
}

// detChunk is one piece of the buffered output, a sequence of records:
// a 1-byte fd, a 4-byte little-endian length, then that many bytes.
type detChunk struct {
	_    sys.NotInHeap
	next *detChunk
	n    int // bytes of data in use
	data [64<<10 - 16]byte
}

var detOut struct {
	detChunks
	flushed   bool // output has been written; write directly from now on
	onFailure bool // write it only if the exit status is nonzero; see detsimBranch

	// share, in a forked branch, is memory shared with the timeline,
	// which takes the branch's output from it if the branch fails (see
	// detsimBranch): everything the branch writes to fds 1 and 2 goes
	// there instead.
	share *detShare
	// branch is the number of the branch whose output the timeline took,
	// which heads the buffered output, or 0.
	branch int
	// late holds the output of the branches that failed after the
	// timeline had, which is written after the timeline's report.
	late detChunks
}

// detChunks is a list of chunks of buffered output.
type detChunks struct {
	head, tail *detChunk
}

// add appends a record of p, written to fd, to l. It does not allocate
// from the heap.
func (l *detChunks) add(fd byte, p []byte) {
	for rest := p; len(rest) > 0; {
		c := l.tail
		if c == nil || len(c.data)-c.n < 6 {
			c = (*detChunk)(persistentalloc(unsafe.Sizeof(detChunk{}), 8, &memstats.other_sys))
			if c == nil {
				throw("detsched: out of memory for the buffered output")
			}
			if l.tail == nil {
				l.head = c
			} else {
				l.tail.next = c
			}
			l.tail = c
		}
		n := min(len(rest), len(c.data)-c.n-5)
		c.data[c.n] = fd
		c.data[c.n+1] = byte(n)
		c.data[c.n+2] = byte(n >> 8)
		c.data[c.n+3] = byte(n >> 16)
		c.data[c.n+4] = byte(n >> 24)
		copy(c.data[c.n+5:], rest[:n])
		c.n += 5 + n
		rest = rest[n:]
	}
}

// A detShare holds a forked branch's output for the timeline that forked
// it, in records as in a detChunk: the first n bytes of data, or all of
// data if n is larger, as when cut is set because a record did not fit.
// It is mapped shared before the fork (see detsimBranch).
type detShare struct {
	n    atomic.Uint64
	cut  atomic.Uint32
	data [detShareSize - 16]byte
}

// detShareSize is the size of a branch's output the timeline takes, at
// most. Only the pages the branch writes take memory.
const detShareSize = 64 << 20

// write appends a record of n bytes at p, written to fd, to s.
//
//go:nosplit
func (s *detShare) write(fd uintptr, p unsafe.Pointer, n int) {
	need := uint64(5 + n)
	off := s.n.Add(int64(need)) - need
	if off+need > uint64(len(s.data)) {
		s.cut.Store(1)
		return
	}
	b := s.data[off : off+need]
	b[0] = byte(fd)
	b[1] = byte(n)
	b[2] = byte(n >> 8)
	b[3] = byte(n >> 16)
	b[4] = byte(n >> 24)
	memmove(unsafe.Pointer(&b[5]), p, uintptr(n))
}

// detOutTakeBranch puts the output that failing branch n left in s at the
// head of the buffered output, as it would have come first had the branch
// written it, so that every process forked later, such as a cell of a
// search, writes it with the failure the branch left.
func detOutTakeBranch(s *detShare, n int, late bool) {
	var l detChunks
	end := min(s.n.Load(), uint64(len(s.data)))
	for i := uint64(0); i+5 <= end; {
		fd := s.data[i]
		k := uint64(s.data[i+1]) | uint64(s.data[i+2])<<8 | uint64(s.data[i+3])<<16 | uint64(s.data[i+4])<<24
		if fd != 1 && fd != 2 || i+5+k > end {
			break // a write the branch did not finish, or room it did not fill
		}
		l.add(fd, s.data[i+5:i+5+k])
		i += 5 + k
	}
	if s.cut.Load() != 0 {
		l.add(2, []byte("detsched: the branch's output was cut at 64 MiB\n"))
	}
	if l.head == nil {
		return
	}
	if late {
		if detOut.late.tail == nil {
			detOut.late.head = l.head
		} else {
			detOut.late.tail.next = l.head
		}
		detOut.late.tail = l.tail
		return
	}
	l.tail.next = detOut.head
	detOut.head = l.head
	if detOut.tail == nil {
		detOut.tail = l.tail
	}
	detOut.branch = n
}

// syscall_detStartProcess reports that the running goroutine starts the
// program argv0, if the runtime reports (see detReporting): when the
// subprocess's output and exit arrive depends on the OS.
//
//go:linkname syscall_detStartProcess syscall.detStartProcess
func syscall_detStartProcess(argv0 string) {
	if !detReporting() {
		return
	}
	what := "starts a subprocess (" + argv0 + "), whose timing comes from the OS"
	gp := getg()
	systemstack(func() { detStrictOS(what, gp) })
}

// os_runtime_detFileOpen counts fd, a file package os opened, which os
// uncounts unless it is a regular file, and reports whether it did: a forked child shares the file and its
// offset with its parent and its siblings, so detForkCheck refuses to fork
// while one is open. A simulated file is the simulation's, which forks
// with the process.
//
//go:linkname os_runtime_detFileOpen os.runtime_detFileOpen
func os_runtime_detFileOpen(fd int) bool {
	if detsimFD(uintptr(fd)) {
		return false
	}
	atomic.Xaddint32(&detFork.files, 1)
	return true
}

// os_runtime_detOSRand returns a random number from the OS, for package
// os to name a temporary file when the name the seed gave exists.
//
//go:linkname os_runtime_detOSRand os.runtime_detOSRand
func os_runtime_detOSRand() uint64 {
	var buf [8]byte
	b := (*[8]byte)(noescape(unsafe.Pointer(&buf)))
	if readRandom(b[:]) != len(b) {
		readTimeRandom(b[:])
	}
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// os_runtime_detWroteFS records that package os created a directory (fd
// -1) or opened fd for writing, unless the simulation handled it: from then
// on detForkCheck refuses to fork, since a forked child would share with
// its parent and its siblings what is on the real file system, such as a
// temporary directory and the files it writes there.
//
//go:linkname os_runtime_detWroteFS os.runtime_detWroteFS
func os_runtime_detWroteFS(fd int) {
	if fd >= 0 && detsimFD(uintptr(fd)) || fd < 0 && detsimNodeOf() != 0 {
		return
	}
	atomic.Storeint32(&detFork.wroteFS, 1)
}

// os_runtime_detFileClose uncounts a file os_runtime_detFileOpen counted.
//
//go:linkname os_runtime_detFileClose os.runtime_detFileClose
func os_runtime_detFileClose() {
	atomic.Xaddint32(&detFork.files, -1)
}

// syscall_detWrite stores p as output to fd 1 or 2 and returns len(p)
// (or what the real write returned, after detOutputFlush). Only the
// goroutine holding the single P calls it, so acquirem is enough to
// keep calls from interleaving.
//
//go:linkname syscall_detWrite syscall.detWrite
func syscall_detWrite(fd int, p []byte) int {
	if len(p) == 0 {
		return 0
	}
	mp := acquirem()
	defer releasem(mp)
	if detOut.flushed {
		if detOut.share != nil {
			detOut.share.write(uintptr(fd), unsafe.Pointer(&p[0]), len(p))
			return len(p)
		}
		return int(write1(uintptr(fd), unsafe.Pointer(&p[0]), int32(len(p))))
	}
	detOut.add(byte(fd), p)
	return len(p)
}

// detOutputFlush writes the buffered output to the real fds, given exit
// status code. Called by detExit and detPanic. It does not allocate.
func detOutputFlush(code int) {
	if !goexperiment.DetSched || detOut.flushed {
		return
	}
	detOut.flushed = true
	if detOut.onFailure && code == 0 {
		return
	}
	detOut.write()
}

// write writes the output l holds.
func (l *detChunks) write() {
	for c := l.head; c != nil; c = c.next {
		for i := 0; i < c.n; {
			fd := uintptr(c.data[i])
			n := int(c.data[i+1]) | int(c.data[i+2])<<8 | int(c.data[i+3])<<16 | int(c.data[i+4])<<24
			i += 5
			detWriteAll(fd, c.data[i:i+n])
			i += n
		}
	}
}

// detWriteAll writes all of b to fd, retrying for a while if fd is
// nonblocking and full, or in a forked branch, to the memory it shares
// with the timeline. It does not allocate.
func detWriteAll(fd uintptr, b []byte) {
	if detOut.share != nil {
		if len(b) > 0 {
			detOut.share.write(fd, unsafe.Pointer(&b[0]), len(b))
		}
		return
	}
	for j, retries := 0, 0; j < len(b) && retries < 1000; {
		w := write1(fd, unsafe.Pointer(&b[j]), int32(len(b)-j))
		if w <= 0 {
			retries++ // for example, EAGAIN on a nonblocking fd
			usleep(1000)
			continue
		}
		j += int(w)
	}
}

// detTraceTime records in the current P's schedule hash that bubble's
// clock moved to bubble.now. Called by synctestRun.
func detTraceTime(bubble *synctestBubble) {
	pp := getg().m.p.ptr()
	detP(pp).trace = detMix(detMix(detP(pp).trace, bubble.id), uint64(bubble.now))
	detFeedMaxOf(detFeedMaxTime, uint64(bubble.now-946684800000000000)) // since synctest's base time
	if debug.dettrace >= 2 {
		print("detsched: bubble ", bubble.id, " time ", bubble.now, "\n")
	}
}

// detBubbles lists the running synctest bubbles, oldest first, linked
// through detNext. Only code running on the single P reads or writes it.
var detBubbles *synctestBubble

func detBubblesAdd(bubble *synctestBubble) {
	pb := &detBubbles
	for *pb != nil {
		pb = &(*pb).detNext
	}
	*pb = bubble
}

func detBubblesRemove(bubble *synctestBubble) {
	for pb := &detBubbles; *pb != nil; pb = &(*pb).detNext {
		if *pb == bubble {
			*pb = bubble.detNext
			bubble.detNext = nil
			return
		}
	}
}

// detSynctestIdle applies the rule described at the top of this file.
// findRunnable calls it, holding the P, when it found nothing to run.
// It returns a goroutine it made runnable for findRunnable to run (the
// bubble's root goroutine, or its goroutine blocked in synctest.Wait), or
// nil if the rule does not apply.
func detSynctestIdle() *g {
	if detBubbles == nil || netpollinited() && netpollAnyWaiters() && detsimRealPollWaiters() {
		return nil
	}
	for mp := (*m)(atomic.Loadp(unsafe.Pointer(&allm))); mp != nil; mp = mp.alllink {
		if gp := mp.curg; gp != nil && readgstatus(gp)&^_Gscan == _Gsyscall {
			return nil // the OS may still unblock something
		}
	}

	// Pick one bubble. Ranks: 0, Wait returns or a due timer fires
	// (no time passes); 1, the clock jumps to when; 2, deadlock.
	var best *synctestBubble
	bestRank, bestWhen := 0, int64(0)
	for b := detBubbles; b != nil; b = b.detNext {
		lock(&b.mu)
		// A bubble with running == 0 is already handled by synctest
		// itself, and active > 0 means something outside the goroutine
		// counts is still going on.
		if b.running > 0 && b.active == 0 {
			rank, when := 2, int64(0)
			next := b.timers.wakeTime()
			switch {
			case b.waiter != nil || next > 0 && next <= b.now:
				rank = 0
			case next > 0 && !b.done:
				rank, when = 1, next
			}
			if best == nil || rank < bestRank ||
				rank == bestRank && (when < bestWhen || when == bestWhen && b.id < best.id) {
				best, bestRank, bestWhen = b, rank, when
			}
		}
		unlock(&b.mu)
	}
	if best == nil {
		return nil
	}
	lock(&best.mu)
	// Wake the bubble as synctest would if its running count were 0.
	running := best.running
	best.running = 0
	gp := best.maybeWakeLocked()
	best.running = running
	unlock(&best.mu)

	trace := traceAcquire()
	casgstatus(gp, _Gwaiting, _Grunnable)
	if trace.ok() {
		trace.GoUnpark(gp, 0)
		traceRelease(trace)
	}
	return gp
}

// detStrictSeen records hashes of the reports detStrictOS already made.
// Only code running on the single P touches it.
var detStrictSeen struct {
	keys    [64]uint64
	n       int
	reports int // reports made
	full    bool
}

// detStrictFirst reports whether key has not been reported before, and
// records it.
func detStrictFirst(key uint64) bool {
	s := &detStrictSeen
	for _, k := range s.keys[:s.n] {
		if k == key {
			return false
		}
	}
	if s.n == len(s.keys) {
		if !s.full {
			s.full = true
			print("detsched: too many distinct reports; not reporting more\n")
		}
		return false
	}
	s.keys[s.n] = key
	s.n++
	s.reports++
	return true
}

// detStrictOS reports that goroutine gp (the running goroutine, or one
// being woken) makes the program depend on the OS or on real time, as what
// describes, and prints gp's stack, if the runtime reports (see
// detReporting). Each report is made once, or each time under dettrace=2,
// which prints every switch. Must run on the system stack.
func detStrictOS(what string, gp *g) {
	if !detReporting() {
		return
	}
	var pcs [32]uintptr
	n := gcallers(gp, 0, pcs[:])
	key := detMix(0, uint64(len(what)))
	for _, pc := range pcs[:n] {
		key = detMix(key, uint64(pc))
	}
	if !detStrictFirst(key) && debug.dettrace < 2 {
		return
	}
	print("detsched: goroutine ", gp.goid, " ", what)
	detStrictAt()
	print("; the program no longer runs deterministically\n")
	traceback(^uintptr(0), ^uintptr(0), 0, gp)
	print("\n")
}

// detStrictTimer reports that a timer outside every synctest bubble is
// about to call f(arg), if the runtime reports (see detReporting), as
// detStrictOS does. Called by (*timer).unlockAndRun, on the system stack.
func detStrictTimer(f func(any, uintptr, int64), arg any) {
	if !detReporting() {
		return
	}
	fpc := **(**uintptr)(unsafe.Pointer(&f))
	if fpc == abi.FuncPCABIInternal(goroutineReady) {
		detStrictOS("wakes from a sleep outside any synctest bubble, after real time passed", arg.(*g))
		return
	}
	if !detStrictFirst(detMix(1, uint64(fpc))) && debug.dettrace < 2 {
		return
	}
	print("detsched: a timer outside any synctest bubble fired after real time passed and calls ", funcname(findfunc(fpc)))
	detStrictAt()
	print("; the program no longer runs deterministically\n\n")
}

// detStrictAt prints where in the run a report is made, as GODETSTOP's
// points count it: the switch and the tick. A report from the same seed
// at another point means real time decided where the runs part.
func detStrictAt() {
	if pp := getg().m.p.ptr(); pp != nil {
		print(", at s", detP(pp).switches, " k", detTickNow())
	}
}

// detSyscallRetakeNS is how long a goroutine stays in one system call
// before detWatchdog hands off its P. detWatchdog checks this often.
// During package init it waits detInitSyscallRetakeNS instead: init makes
// system calls that never wait for another goroutine, such as getcwd in
// package os and sysctl in package syscall on darwin, and on a loaded
// machine one of them can take 10ms; a hand-off there would make the run
// take another schedule before main.
const (
	detSyscallRetakeNS     = 10 * 1000 * 1000
	detInitSyscallRetakeNS = 1000 * 1000 * 1000
)

// detWatchdog is the det-mode replacement for sysmon. It runs on its own
// M without a P, and only hands off the P of a goroutine that stays in
// a system call (see the comment at the top of this file).
func detWatchdog() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)
	detFork.watchdog.set(getg().m)
	detWatchdogLoop()
}

// detWatchdogLoop is the watchdog's loop, where a forked child restarts
// it. It holds detFork.lock while it checks, so that a fork never copies
// a lock it holds.
func detWatchdogLoop() {
	for {
		usleep(detSyscallRetakeNS / 1000)
		lock(&detFork.lock)
		detRetakeSyscall(nanotime())
		unlock(&detFork.lock)
	}
}

// detRetakeSyscall hands off the single P if its goroutine has been in
// the same system call since at least detSyscallRetakeNS before now. It
// is the system call part of retake, without preempting running code.
func detRetakeSyscall(now int64) {
	lock(&allpLock)
	pp := allp[0]
	unlock(&allpLock)
	if atomic.Load(&pp.status) != _Prunning {
		return
	}
	pd := &pp.sysmontick

	// As in retake: pretend one more M is running while we may hold
	// the P, so that the M in the system call can't report a deadlock.
	incidlelocked(-1)
	thread, ok := setBlockOnExitSyscall(pp)
	if ok {
		if syst := pp.syscalltick; pd.syscalltick != syst || pd.syscallwhen == 0 {
			// A different system call than last time, or the first one
			// seen, whose count may be the initial 0: start timing it.
			pd.syscalltick = syst
			pd.syscallwhen = now
			thread.resume()
		} else if d := now - pd.syscallwhen; d < detSyscallRetakeNS || d < detInitSyscallRetakeNS && !mainInitDone.Load() {
			thread.resume()
		} else {
			// The goroutine is held in _Gsyscall and the P is ours, so
			// no Go code runs while we report.
			thread.takeP()
			detStrictOS("has been in a system call for at least 10ms of real time, so other goroutines run meanwhile", thread.gp)
			thread.resume()
			handoffp(pp)
		}
	}
	incidlelocked(1)
}

// Scheduling perturbation.
//
// A random walk or PCT makes the choices that Go leaves to the scheduler,
// so that different seeds explore different interleavings. Each run is
// still deterministic: the choices come from the P's sched stream, seeded
// from GODETSEED. Perturbation is on only when GODETSEED is set. Each run
// draws its strategy, walk or PCT with equal
// chance, and then the strategy's parameters (swarm testing), so that some
// runs have long quanta or never reorder, and others preempt and reorder
// constantly. GODETSEARCH's perturb field can pick one strategy or turn
// perturbation off. The trace report names the strategy and parameters:
//
//	detsched: perturb walk quantum=Q pick=N/16 wake=N/16 sync=N/16
//	detsched: perturb pct depth=D steps=K
//
// The random walk chooses:
//
//   - each goroutine's quantum, drawn from an exponential distribution
//     with mean detPerturb.quantum ticks, instead of detQuantum;
//   - which goroutine runs next: with chance detPerturb.pick, findRunnable
//     runs one picked uniformly from the local and global run queues
//     (see detPickRunnable);
//   - with chance detPerturb.wake, a goroutine that makes another one
//     runnable yields at its next tick, as if the other had started at
//     once on another P (see detWake);
//   - with chance about detPerturb.sync, a goroutine yields at a
//     synchronization point, just before a channel operation, select, go
//     statement, or atomic operation (see detSync).
//
// PCT is Burckhardt et al., "A Randomized Scheduler with Probabilistic
// Guarantees of Finding Bugs", ASPLOS 2010. Each goroutine gets a random
// priority the first time the scheduler sees it, and the highest-priority
// runnable goroutine always runs: findRunnable picks it, and a goroutine
// that makes a higher-priority one runnable yields at its next tick. At
// d-1 change points, synchronization steps drawn uniformly from 1 to k,
// the running goroutine's priority drops to d-i for the i'th one, below
// every priority drawn at random. A run finds a bug that needs d
// particular orderings with probability at least 1/(n*k^(d-1)) for n
// goroutines, if k is about the number of synchronization points the run
// passes. Nothing measures that, so each run draws d from 1 to 5 and k
// from powers of two times 100, up to about 6.5 million, which covers
// short and long runs alike.
//
// Strict priorities livelock when a goroutine spins, even with Gosched
// or atomic loads, waiting for a lower-priority one. So a goroutine that
// runs detQuantum ticks in total at one priority, across yields, drops
// below every other goroutine.
//
// The random walk finds bugs that need many small reorderings, and PCT
// bugs that need a few particular orderings among few synchronization
// steps, so each finds bugs the other rarely does. Over many seeds,
// drawing the strategy finds each bug at least half as often as the
// better strategy for it.

// detPerturb holds the perturbation parameters. It is zero when
// perturbation is off.
var detPerturb struct {
	quantum int32  // mean quantum in ticks
	pick    uint32 // chance in 16 that findRunnable picks a random goroutine
	wake    uint32 // chance in 16 that making a goroutine runnable yields
	sync    uint32 // about the chance in 16 of a yield at a synchronization point
	pct     bool   // use PCT instead of the random walk
}

// detPCT is the state of PCT.
var detPCT struct {
	depth   int32
	steps   int64
	changes [detPCTMaxDepth - 1]int64 // synchronization steps of the change points, sorted
	next    int32                     // index of the next change point
	lowest  int64                     // priority of the goroutine demoted last
	keep    bool                      // the next preemption keeps the running goroutine's priority
	last    *g                        // the goroutine detPCTExecute started last
	lastQ   int32                     // the ticks it started with
}

const detPCTMaxDepth = 5

// detPerturbStrategy is the perturbation strategy: 0, off; 1, the random
// walk; 2, PCT; 3, drawn per run, the default when GODETSEED is set. See
// detSearchInit.
var detPerturbStrategy int32 = 3

// detPerturbInit sets detPerturb, drawing the strategy and its parameters
// from pp's sched stream. cur is the current synchronization step, and
// steps, if not 0, is the step PCT draws its change points before. Called
// by detRandInit, and again at each branch point by detBranch.
func detPerturbInit(pp *p, cur, steps int64) {
	detPerturb.quantum, detPerturb.pick, detPerturb.wake, detPerturb.sync, detPerturb.pct = 0, 0, 0, 0, false
	detSetSyncs(1<<31 - 1)
	strategy := detPerturbStrategy
	if strategy == 3 {
		strategy = int32(detSchedRand(pp)&1) + 1
	}
	switch strategy {
	case 0:
		return
	case 2:
		detPCTInit(pp, cur, steps)
		return
	}
	quanta := [...]int32{10, 100, 1000, 10000, 100000}
	chances := [...]uint32{0, 1, 4, 8, 16}
	detPerturb.quantum = quanta[detSchedRand(pp)%uint64(len(quanta))]
	detPerturb.pick = chances[detSchedRand(pp)%uint64(len(chances))]
	detPerturb.wake = chances[detSchedRand(pp)%uint64(len(chances))]
	detPerturb.sync = chances[detSchedRand(pp)%uint64(len(chances))]
	detSyncGap(pp)
}

// detSchedRand returns the next value of pp's sched stream.
func detSchedRand(pp *p) uint64 {
	c := &detP(pp).sched
	for {
		if x, ok := c.Next(); ok {
			return x
		}
		mp := acquirem() // keep the P while c.Refill may do stack split checks
		c.Refill()
		releasem(mp)
	}
}

// detChance reports whether an event with the given chance in 16 happens.
func detChance(pp *p, chance uint32) bool {
	return chance > 0 && uint32(detSchedRand(pp)&15) < chance
}

// detExpRand returns a sample from an exponential distribution with the
// given mean, at least 1, drawn from pp's sched stream (see fastexprand).
func detExpRand(pp *p, mean int32) int32 {
	const randomBitCount = 26
	q := uint32(detSchedRand(pp)&(1<<randomBitCount-1)) + 1
	qlog := fastlog2(float64(q)) - randomBitCount
	if qlog > 0 {
		qlog = 0
	}
	const minusLog2 = -0.6931471805599453 // -ln(2)
	return int32(qlog*(minusLog2*float64(mean))) + 1
}

// detPickRunnable is called by findRunnable before it takes a goroutine
// from the run queues. With chance detPerturb.pick, it removes and
// returns a goroutine picked uniformly from pp's local run queue and the
// global run queue; the others keep their order. Det mode never uses
// runnext (runqput clears next when there is no sysmon), and with one P
// no other P touches pp's run queue.
func detPickRunnable(pp *p) *g {
	// Draw nothing when no goroutine is runnable: findRunnable comes back
	// here each time the OS wakes it while the program waits in real
	// time, and a draw per wake-up would shift every later choice.
	lock(&sched.lock)
	empty := pp.runqtail == atomic.LoadAcq(&pp.runqhead) && sched.runq.size == 0
	unlock(&sched.lock)
	if empty || !detPerturb.pct && !detChance(pp, detPerturb.pick) {
		return nil
	}
	h := atomic.LoadAcq(&pp.runqhead)
	nl := pp.runqtail - h
	lock(&sched.lock)
	n := nl + uint32(sched.runq.size)
	if n == 0 {
		unlock(&sched.lock)
		return nil
	}
	var i uint32
	if detPerturb.pct {
		i = detPCTPick(pp, h, nl)
	} else {
		i = uint32(uint64(uint32(detSchedRand(pp))) * uint64(n) >> 32)
	}
	if i < nl {
		unlock(&sched.lock)
		size := uint32(len(pp.runq))
		gp := pp.runq[(h+i)%size].ptr()
		for ; i > 0; i-- {
			pp.runq[(h+i)%size] = pp.runq[(h+i-1)%size]
		}
		atomic.StoreRel(&pp.runqhead, h+1)
		return gp
	}
	var prev *g
	gp := sched.runq.head.ptr()
	for i -= nl; i > 0; i-- {
		prev, gp = gp, gp.schedlink.ptr()
	}
	if prev == nil {
		sched.runq.head = gp.schedlink
	} else {
		prev.schedlink = gp.schedlink
	}
	if sched.runq.tail.ptr() == gp {
		sched.runq.tail.set(prev)
	}
	gp.schedlink = 0
	sched.runq.size--
	unlock(&sched.lock)
	return gp
}

// detSyncs is the number of synchronization points left before the
// running goroutine yields at one. Compiled code decrements it; see the
// comment at the top of this file. It stays far from zero unless
// perturbation is on.
var detSyncs int32 = 1<<31 - 1

// detSyncEnd is the synchronization step at which detSyncs reaches zero.
var detSyncEnd int64 = 1<<31 - 1

// detSyncStep returns the number of synchronization points passed so far.
func detSyncStep() int64 { return detSyncEnd - int64(detSyncs) }

// detSetSyncs makes compiled code call detSync after n more
// synchronization points.
func detSetSyncs(n int32) {
	detSyncEnd = detSyncStep() + int64(n)
	detSyncs = n
}

// detSync is called by compiled code when detSyncs runs out, just before
// a synchronization point. It draws the gap to the next yield and yields
// the P, as detPreempt does.
func detSync() {
	if detPerturb.pct {
		detPCTChange()
		return
	}
	if detPerturb.sync == 0 {
		detSetSyncs(1<<31 - 1)
		return
	}
	detSyncGap(getg().m.p.ptr())
	detPreempt()
}

// detSyncGap sets detSyncs to the number of synchronization points before
// the next yield: every one when detPerturb.sync is 16/16, else drawn from
// an exponential distribution with mean 16/detPerturb.sync.
func detSyncGap(pp *p) {
	switch detPerturb.sync {
	case 0:
		detSetSyncs(1<<31 - 1)
	case 16:
		detSetSyncs(1)
	default:
		detSetSyncs(detExpRand(pp, int32(16/detPerturb.sync)))
	}
}

// detWake is called by runqput when gp becomes runnable. With chance
// detPerturb.wake, or under PCT if gp has a higher priority than the
// running goroutine, it makes the running goroutine yield at its next
// tick.
func detWake(pp *p, gp *g) {
	if detPerturb.pct {
		detPCTWake(pp, gp)
		return
	}
	if detChance(pp, detPerturb.wake) {
		detSetTicks(1)
	}
}

// Memory outside the heap.
//
// The heap's addresses come from GODETSEED, but the runtime's other
// memory, such as spans, persistentalloc blocks, and the output buffer,
// is placed by the OS, at random addresses under ASLR. Pointers to it pass
// through runtime frames on goroutine stacks, and a traceback prints stale
// stack words as possibly inaccurate arguments, so a crash's output could
// differ between runs. In det mode sysAlloc instead asks for addresses
// counted up from detSysBase, far from the heap's hints and from where
// Linux places mmaps. If the OS gives another address, the memory is used
// anyway.

// detSysNext is the next address detSysAlloc asks for.
var detSysNext atomic.Uintptr

const detSysBase = 0x7c00 << 32 * (goarch.PtrSize / 8) // 0 with 32-bit pointers, where detSysAlloc is not used

// detSysAlloc is sysAllocOS in det mode.
func detSysAlloc(n uintptr, vmaName string) unsafe.Pointer {
	if goarch.PtrSize != 8 {
		return sysAllocOS(n, vmaName)
	}
	size := alignUp(n, 64<<10) // physPageSize may not be set yet
	detSysNext.CompareAndSwap(0, detSysBase)
	v := unsafe.Pointer(detSysNext.Add(size) - size)
	p := sysReserveOS(v, size, vmaName)
	if p != v {
		if p != nil {
			sysFreeOS(p, size)
		}
		return sysAllocOS(n, vmaName)
	}
	sysMapOS(p, size, vmaName)
	return p
}

// PCT. See the comment above detPerturb.

// detPCTInit sets up PCT. Called by detPerturbInit.
func detPCTInit(pp *p, cur, steps int64) {
	detPerturb.pct = true
	d := int32(detSchedRand(pp)%5) + 1
	k := int64(1) << (detSchedRand(pp) % 24) // log-uniform from 1 to 8M synchronization steps
	if steps > 0 {
		k = steps
	}
	detPCT.depth, detPCT.steps = d, k
	detPCTDrawChanges(pp, cur)
}

// detPCTDrawChanges draws d-1 change points uniformly from the
// synchronization steps after step cur, up to the k steps an unperturbed
// run passes, and makes detSync run at the first. A run that has passed
// k already draws them from the next k.
func detPCTDrawChanges(pp *p, cur int64) {
	k := detPCT.steps - cur
	if k <= 0 {
		k = detPCT.steps
	}
	detPCT.next = 0
	for i := int32(0); i < detPCT.depth-1; i++ {
		c := cur + 1 + int64(detSchedRand(pp)%uint64(k))
		j := i
		for ; j > 0 && detPCT.changes[j-1] > c; j-- {
			detPCT.changes[j] = detPCT.changes[j-1]
		}
		detPCT.changes[j] = c
	}
	detPCTNextChange()
}

// detPCTNextChange makes detSync run at the next change point.
func detPCTNextChange() {
	if detPCT.next >= detPCT.depth-1 {
		detSetSyncs(1<<31 - 1)
		return
	}
	detSetSyncs(int32(max(detPCT.changes[detPCT.next]-detSyncStep(), 1)))
}

// detPCTChange is detSync under PCT: the running goroutine is at a change
// point, so its priority drops to d-i for the i'th change point, and it
// yields so that the highest-priority goroutine runs.
func detPCTChange() {
	detPCT.next++
	detPCTNextChange()
	gp := getg().m.curg
	if gp == nil {
		return
	}
	s := detPCTState(gp.m.p.ptr(), gp)
	s.prio, s.used = int64(detPCT.depth-detPCT.next), 0
	detPCT.keep = true
	detPreempt()
}

// detPCTState returns gp's state, drawing its priority from pp's sched
// stream the first time.
func detPCTState(pp *p, gp *g) *detGState {
	s := detG(gp)
	if s.goid != gp.goid {
		// The node and the held mutexes (see heldGoid) stay: PCT can
		// start in the middle of a run.
		s.goid, s.prio, s.used = gp.goid, 1<<32+int64(uint32(detSchedRand(pp))), 0
	}
	return s
}

// detPCTExecute is called by detExecute when gp starts running. It
// charges the ticks the last goroutine ran to its budget, and gives gp
// the rest of its own.
func detPCTExecute(pp *p, gp *g) {
	if last := detPCT.last; last != nil && detG(last).goid == last.goid {
		detG(last).used += max(detPCT.lastQ-detTicksReal(), 0)
	}
	s := detPCTState(pp, gp)
	detPutTicks(max(detQuantum-s.used, 1))
	detPCT.last, detPCT.lastQ, detPCT.keep = gp, detTicksReal(), false
}

// detPCTPreempt is called when gp is preempted by its ticks. Unless a
// higher-priority goroutine became runnable or gp just passed a change
// point, gp used up its budget, and it drops below every goroutine.
func detPCTPreempt(gp *g) {
	if detPCT.keep {
		return
	}
	s := detG(gp)
	s.prio, s.used = detPCT.lowest, 0
	detPCT.lowest--
}

// detPCTWake is detWake under PCT: if gp has a higher priority than the
// running goroutine, the running goroutine yields at its next tick.
func detPCTWake(pp *p, gp *g) {
	cur := getg().m.curg
	if cur == nil || cur == gp || detPCTState(pp, gp).prio <= detPCTState(pp, cur).prio {
		return
	}
	if detPCT.last == cur {
		detG(cur).used += max(detPCT.lastQ-detTicksReal(), 0)
	}
	detSetTicks(1)
	detPCT.lastQ, detPCT.keep = 1, true
}

// detPCTPick returns the index of the highest-priority goroutine in pp's
// local run queue, which has nl goroutines from head h, followed by the
// global run queue. The caller holds sched.lock.
func detPCTPick(pp *p, h, nl uint32) uint32 {
	var best, i uint32
	bestPrio := int64(-1 << 63)
	consider := func(gp *g) {
		if p := detPCTState(pp, gp).prio; p > bestPrio {
			best, bestPrio = i, p
		}
		i++
	}
	for j := uint32(0); j < nl; j++ {
		consider(pp.runq[(h+j)%uint32(len(pp.runq))].ptr())
	}
	for gp := sched.runq.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
		consider(gp)
	}
	return best
}

// The tape.
//
// A run is determined by GODETSEED and its tape, GODETSEARCH's tape field:
// a comma-separated list of branch points N:S or N:S:K with increasing
// switch numbers N. At switch N, the number dettrace=2 prints, the
// runtime reseeds the P's random state and sched stream from (S, N)
// before the switch's goroutine draws anything. So a run with a tape
// executes exactly like the run without it up to switch N, and then
// makes different random choices: map order, select, math/rand,
// crypto/rand, and, with perturbation, the schedule. Perturbation keeps
// its parameters. The random walk draws a new gap to its next sync
// yield, and PCT draws a fresh set of d-1 change points over the k
// steps after the branch, or the K steps a branch point gives, keeping
// the priorities it gave out. The trace report prints the tape.

type detBranchPoint struct {
	switchN, seed uint64
	steps         int64 // PCT's horizon after the branch (see detPerturbInit), or 0
}

// detTape holds the tape's branch points, and detTapeNext the index of
// the next one.
var (
	detTape     []detBranchPoint
	detTapeNext int
)

// detTapeInit parses the tape s. Called by detSearchInit.
func detTapeInit(s string) {
	n := 1
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			n++
		}
	}
	detTape = unsafe.Slice((*detBranchPoint)(persistentalloc(uintptr(n)*unsafe.Sizeof(detBranchPoint{}), 8, &memstats.other_sys)), n)
	k := 0
	for len(s) > 0 {
		item := s
		if i := bytealg.IndexByteString(s, ','); i >= 0 {
			item, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		i := bytealg.IndexByteString(item, ':')
		var b detBranchPoint
		ok1, ok2, ok3 := false, false, true
		if i >= 0 {
			b.switchN, ok1 = atoi64u(item[:i])
			rest := item[i+1:]
			if j := bytealg.IndexByteString(rest, ':'); j >= 0 {
				var k uint64
				k, ok3 = atoi64u(rest[j+1:])
				b.steps, rest = int64(k), rest[:j]
				ok3 = ok3 && b.steps > 0
			}
			b.seed, ok2 = atoi64u(rest)
		}
		if !ok1 || !ok2 || !ok3 || b.switchN == 0 || k > 0 && b.switchN <= detTape[k-1].switchN {
			fatal("GODETSEARCH's tape must be a list of branch points N:S or N:S:K with increasing switch numbers N >= 1")
		}
		detTape[k] = b
		k++
	}
}

// atoi64u parses a decimal uint64.
func atoi64u(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		d := uint64(s[i] - '0')
		if d > 9 || n > (1<<64-1-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// detBranch applies the next branch point at the current switch: it
// reseeds the streams and draws the perturbation strategy and its
// parameters again, so that each branch explores as a fresh seed would.
// A branch point with detBranchGoesOn changes nothing.
func detBranch(pp *p) {
	b := detTape[detTapeNext]
	detTapeNext++
	if b.seed&detBranchGoesOn != 0 {
		return
	}
	detBranches++
	detBranchSwitch = b.switchN
	detSeedStreams(pp, b.seed, b.switchN<<16|uint64(pp.id))
	detPerturbInit(pp, detSyncStep(), b.steps)
}

// Setup.
//
// A program calls lifecycle.SetupComplete in detsched/sdk when its
// expensive setup is done. The runtime records the switch, which the trace
// reports, and yields, so that the next switch comes before any code after
// the call runs: a driver that branches there explores everything after
// setup.

// detSetup is the switch running when setup completed, or 0.
var detSetup uint64

// A branch point whose seed has detBranchGoesOn set goes on with the run
// it branches from, making the same random choices it would have made, as
// detcheck -guide does after a run stops where it found something new: the
// stopped run stays as a cell to branch from, and the run itself goes on
// unchanged, as if it had not stopped. New random choices there would
// redraw the run's perturbation and its nemesis's plan at every new state
// it reaches, breaking up the long faults some bugs need. lifecycle.Branches
// does not count it.
const detBranchGoesOn = 1 << 63

// detBranches counts the branch points taken without detBranchGoesOn, and
// detBranchSwitch is the switch of the last one.
var (
	detBranches     int
	detBranchSwitch uint64
)

//go:linkname detSDKBranches github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle.runtime_branches
func detSDKBranches() int {
	return detBranches
}

//go:linkname detSDKSetupComplete github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle.runtime_setupComplete
func detSDKSetupComplete() {
	if detSetup != 0 {
		return
	}
	detSetup = detP(getg().m.p.ptr()).switches
	mcall(gosched_m)
}

// Fork mode.
//
// With GODETSEARCH's fork=FD, the runtime serves a driver over FD, a Unix
// SOCK_SEQPACKET socket, so that one execution can continue as many
// branches without re-running the shared prefix. The process stops at its
// first switch and at each switch the driver names, before the tape is
// applied there. At a stop the driver can fork a child that continues with
// the branch point (switch, seed) added to its tape, its fds 1 and 2 and
// optionally its own control socket replaced by descriptors the driver
// passes; wait for a child; run on to a later switch; or exit. A child
// runs exactly as re-executing its tape would, so the tape alone
// reproduces it.
//
// fork keeps only the calling thread. In det mode that is the only thread
// running Go code, and a fork is refused unless every other M is idle or
// the watchdog; the child starts them on new threads. See detfork.go, and
// detfork_linux.go and detfork_darwin.go, which implement it for Linux on
// amd64 and arm64 and for macOS on arm64.

var detFork struct {
	fd       int32  // control socket, or 0
	stop     uint64 // switch to stop at next, or 0
	branched uint64 // switch this process was forked at, or 0
	lock     mutex  // held by the watchdog while it checks, and while forking
	watchdog muintptr
	pollOpen int32 // descriptors registered with the network poller
	files    int32 // regular files package os has open (see os_runtime_detFileOpen)
	wroteFS  int32 // package os wrote to the real file system (see os_runtime_detWroteFS)
	security bool  // Security.framework was called (darwin); see detfork_darwin.go
}

// detBranchChild reports whether this process is a branch that ends the
// timeline, and detBranchN counts the branches the timeline reached up to
// it, forked or not; see detsimBranch. With GODETBRANCH=N, the timeline
// runs its Nth branch in place instead of forking.
var (
	detBranchChild bool
	detBranchN     int
)

// The environment.
//
// The environment is copied into the heap at startup, and its size would
// then decide heap sizes and addresses, and so where GC runs and the
// schedule: a run from another shell, which differs only in $_, would
// take a different schedule. In det mode the runtime's copy (detGoenvs),
// and package syscall's copy and lookup map (detEnvsCopy), have a fixed
// size of detEnvCap variables and detEnvBytes bytes, so the environment
// changes the heap only through what the program does with it. The copy
// stays in the heap so that its addresses are deterministic too: memory
// outside the heap is placed by the OS, and a stale pointer to it on a
// stack can show in a traceback. Package syscall counts no ticks (see
// ssagen.detPreempt), so its loops over the whole environment do not move
// preemptions either. A larger environment is still
// deterministic, but its size matters again.

const (
	detEnvCap   = 1024     // variables
	detEnvBytes = 64 << 10 // bytes of variables
)

// detEnvRead holds the names of the variables the program looked up with
// package syscall's Getenv, such as NO_COLOR, separated by spaces. With
// GODETSEARCH's envread the run prints them at exit, so that a search's
// replay lines can set them as the run had them, or unset them, whatever
// the shell they run in has. The names are copied outside the heap, so
// recording them changes nothing the schedule depends on.
var detEnvRead struct {
	lock   mutex
	report bool
	n      int
	buf    [4096]byte
}

//go:linkname syscall_runtimeEnvRead syscall.runtimeEnvRead
func syscall_runtimeEnvRead(key string) {
	lock(&detEnvRead.lock)
	defer unlock(&detEnvRead.lock)
	for s := unsafe.String(&detEnvRead.buf[0], detEnvRead.n); s != ""; {
		name := s
		if i := bytealg.IndexByteString(s, ' '); i >= 0 {
			name, s = s[:i], s[i+1:]
		} else {
			s = ""
		}
		if name == key {
			return
		}
	}
	n := detEnvRead.n
	if n > 0 {
		n++
	}
	if n+len(key) > len(detEnvRead.buf) || bytealg.IndexByteString(key, ' ') >= 0 {
		return
	}
	if n > 0 {
		detEnvRead.buf[n-1] = ' '
	}
	copy(detEnvRead.buf[n:], key)
	detEnvRead.n = n + len(key)
}

// detEnvHidden is the environment variable that the runtime added to its
// own environment, if any, which the program does not see (see detNoASLR).
var detEnvHidden *byte

// detGoenvs is goenvs_unix in det mode: it copies the n environment
// variables, but detEnvHidden, into allocations of a fixed size.
func detGoenvs(n int32) {
	total, m := 0, 0
	for i := int32(0); i < n; i++ {
		if p := argv_index(argv, argc+1+i); p != detEnvHidden {
			total += findnull(p)
			m++
		}
	}
	envs = make([]string, m, max(m, detEnvCap))
	buf := make([]byte, max(total, detEnvBytes))
	off, j := 0, 0
	for i := int32(0); i < n; i++ {
		p := argv_index(argv, argc+1+i)
		if p == detEnvHidden {
			continue
		}
		l := findnull(p)
		copy(buf[off:], unsafe.Slice(p, l))
		envs[j] = unsafe.String(&buf[off], l)
		off += l
		j++
	}
}

// detEnvsCopy returns package syscall's copy of envs, with capacity for
// detEnvCap variables. syscall's lookup map takes its size hint from the
// copy's capacity.
func detEnvsCopy() []string {
	return append(make([]string, 0, max(len(envs), detEnvCap)), envs...)
}

// detGodebugEnv returns $GODEBUG as package internal/godebug sees it in
// det mode: without the settings whose names start with "det", which only
// the runtime reads. internal/godebug allocates for every setting it
// parses and counts ticks while parsing, so otherwise adding a diagnostic
// setting such as dettrace=2 to replay a failing seed would change the
// schedule it replays. The result is always a new allocation, of the
// length of what remains, so that it does not depend on whether there
// were det settings either.
func detGodebugEnv(s string) string {
	n := 0
	for p := s; p != ""; {
		f, rest, _ := stringslite.Cut(p, ",")
		if f != "" && !stringslite.HasPrefix(f, "det") {
			n += len(f) + 1
		}
		p = rest
	}
	if n == 0 {
		return ""
	}
	r, b := rawstring(n - 1)
	i := 0
	for p := s; p != ""; {
		f, rest, _ := stringslite.Cut(p, ",")
		if f != "" && !stringslite.HasPrefix(f, "det") {
			if i > 0 {
				b[i] = ','
				i++
			}
			i += copy(b[i:], f)
		}
		p = rest
	}
	return r
}
