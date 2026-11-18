// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"internal/goexperiment"
	"internal/stringslite"
	"math/bits"
	"unsafe"
)

// Feedback.
//
// With GODETSEARCH's feedback field set, the runtime records
// features of the run that a search can use to tell which runs reached
// something new. Each feature is a kind and a hash, counted in a
// saturating byte in detFeed.hits, as AFL counts edges. Sites are hashes of
// the innermost return addresses, found by following frame pointers, as
// offsets from the start of the text segment, so they are stable for one
// binary.
//
// Recording does not allocate, does not count ticks (it is runtime code),
// and draws no random numbers, so it does not change the schedule.
//
// feedback=1 prints a summary at exit; any other value is a file that the
// counters are also written to.

const (
	detFeedPark     = iota // a goroutine parked: (site, wait reason)
	detFeedSelect          // a select chose a case: (site, case)
	detFeedComm            // a receive: (last send site on the channel, receive site)
	detFeedLockEdge        // a mutex acquired while holding another: (held site, new site)
	detFeedRecover         // a panic was recovered: (site)
	detFeedMax             // a new power-of-two maximum: (what, log2 of the value)
	detFeedAssert          // an SDK assertion was evaluated: (kind and message, condition)
	detFeedCover           // an edge counter of code compiled with -d=libfuzzer reached a new bucket: (edge, bucket)
	detFeedGuide           // the program reported a state or a new maximum through detsched/sdk/guide
	detFeedKinds
)

// Each kind has 1<<detFeedBits hits, at kind<<detFeedBits, except the
// last, guide states, which has 1<<detFeedGuideBits: a program can report
// tens of thousands of abstract states, such as a cluster's, and with
// fewer slots most new ones would collide with one seen before.
const (
	detFeedBits      = 13
	detFeedGuideBits = 18
	detFeedLen       = detFeedGuide<<detFeedBits + 1<<detFeedGuideBits
)

var detFeedNames = [detFeedKinds]string{"park", "select", "comm", "lockorder", "recover", "max", "assert", "cover", "guide"}

const detFeedLockFrames = 4

var detFeed struct {
	on   bool
	path [256]byte // NUL-terminated file name, or empty
	out  int32     // descriptor a fork-mode child writes its feedback to at exit, or 0

	// In fork mode, the driver's map of features seen so far, and the
	// first feature this process recorded that isn't in it.
	seen        *[detFeedLen]uint8
	novel       uint64 // the feature + 1, or 0
	novelSwitch uint64 // the switch it was recorded in
	stopNovel   bool   // stop at the switch after recording it (detForkRunStopNovel)
	novelStop   bool   // the next stop is for it

	hits     [detFeedLen]uint8
	lastSend [1 << 12]uint64 // site of the last send on a channel, by channel address
	edges    [1 << 16]detLockEdge
	nedges   int

	// Lock acquisition sites from GODETSEARCH's yield field, where a
	// goroutine holds the lock while others run (deadlock-directed
	// scheduling).
	yield  [8]uint64
	nyield int
	hold   uint32 // chance out of 16 that a goroutine holds at a site, or 0 before the first
}

// detFeedHoldYields is how many times a goroutine that acquired a lock at
// a yield site yields while another goroutine can run.
const detFeedHoldYields = 1000

// A detLockEdge records that a goroutine acquired lock b while holding
// lock a: goroutine goid first, and others too if multi. Lock addresses
// are deterministic in det mode.
type detLockEdge struct {
	a, b   uintptr
	goid   uint64
	multi  bool
	pa, pb [detFeedLockFrames]uintptr
}

// A detHeldLock is a mutex the goroutine holds, and where it acquired it.
// In a simulation keep holds the mutex's memory too, unless it is on the
// goroutine's stack: an entry goes stale when another goroutine unlocks
// the mutex, and if its memory were reused for another mutex, a live
// waiter for that one would thaw the stale holder of a crashed node,
// which then runs on in its process's unmapped memory (see detsimThaw).
type detHeldLock struct {
	l    uintptr
	keep unsafe.Pointer
	pcs  [detFeedLockFrames]uintptr
}

// detHeldKeep returns the keep field of a detHeldLock for l, held by gp.
//
//go:nosplit
func detHeldKeep(gp *g, l unsafe.Pointer) unsafe.Pointer {
	if detsim.handler == nil || gp.stack.lo <= uintptr(l) && uintptr(l) < gp.stack.hi {
		return nil
	}
	return l
}

// Code coverage.
//
// With GODETSEARCH's cover field set, packages compiled with -d=libfuzzer
// (as go test -fuzz compiles the package it fuzzes) count each edge in an
// 8-bit counter, and the runtime scans the counters every detCoverEvery
// switches and at exit. Each edge taken is a feature; with cover=counts,
// so is each power-of-two bucket its count reaches, as in AFL, though
// counts grow with the length of a run.

const detCoverEvery = 64

var detCover struct {
	start  *byte
	n      int
	counts bool
	last   [1 << 16]uint8 // the highest bucket of each counter recorded
}

// detFeedCoverInit sets up coverage features, with mode s: 1 or counts.
// Called by detSearchInit.
func detFeedCoverInit(s string) {
	start, stop := unsafe.Pointer(&__start___sancov_cntrs), unsafe.Pointer(&__stop___sancov_cntrs)
	n := int(uintptr(stop) - uintptr(start))
	if uintptr(stop) <= uintptr(start) || n <= 8 {
		return // not instrumented
	}
	detCover.start, detCover.n = (*byte)(start), min(n, len(detCover.last))
	detCover.counts = s == "counts"
	detFeed.on = true
}

// detFeedCoverScan records the counters that reached a new bucket.
func detFeedCoverScan() {
	c := unsafe.Slice(detCover.start, detCover.n)
	for i, v := range c {
		if v == 0 {
			continue
		}
		b := uint8(1)
		if detCover.counts {
			b = uint8(bits.Len8(v))
		}
		if b > detCover.last[i] {
			for k := detCover.last[i] + 1; k <= b; k++ {
				detFeedHit(detFeedCover, detFeedMix(uint64(i)+1, uint64(k)))
			}
			detCover.last[i] = b
		}
	}
}

// detFeedYieldInit sets the lock acquisition sites y, as printed with a
// predicted deadlock, where a goroutine holds the lock while others run.
// It turns feedback on. Called by detSearchInit.
func detFeedYieldInit(y string) {
	for y != "" && detFeed.nyield < len(detFeed.yield) {
		f := y
		if i := stringslite.Index(y, ","); i >= 0 {
			f, y = y[:i], y[i+1:]
		} else {
			y = ""
		}
		f = stringslite.TrimPrefix(f, "0x")
		var h uint64
		for i := 0; i < len(f); i++ {
			c := f[i]
			switch {
			case '0' <= c && c <= '9':
				c -= '0'
			case 'a' <= c && c <= 'f':
				c -= 'a' - 10
			default:
				c = 0
			}
			h = h<<4 | uint64(c)
		}
		detFeed.yield[detFeed.nyield] = h
		detFeed.nyield++
		detFeed.on = true
	}
}

// detFeedInit turns feedback on, with s 1 or the file to write the
// counters to at exit. Called by detSearchInit.
func detFeedInit(s string) {
	detFeed.on = true
	if s != "1" && len(s) < len(detFeed.path) {
		copy(detFeed.path[:], s)
	}
}

// detFeedHit counts feature h of kind.
//
//go:nosplit
func detFeedHit(kind int, h uint64) {
	i := kind<<detFeedBits | int(h>>(64-detFeedBits))
	if kind == detFeedGuide {
		i = kind<<detFeedBits + int(h>>(64-detFeedGuideBits))
	}
	// A process that failed an assertion is not new: as a cell, every
	// run from it would report its failure again.
	if detFeed.hits[i] == 0 && detFeed.seen != nil && detFeed.seen[i] == 0 && detFeed.novel == 0 && detSDK.failures == 0 {
		detFeedNovel(i)
	}
	if detFeed.hits[i] != 255 {
		detFeed.hits[i]++
	}
}

// detFeedNovel records that feature i is the first this process recorded
// that the driver hasn't seen, and stops at the next switch if asked to.
//
//go:nosplit
func detFeedNovel(i int) {
	detFeed.novel = uint64(i) + 1
	if pp := getg().m.p.ptr(); pp != nil {
		detFeed.novelSwitch = detP(pp).switches
		if detFeed.stopNovel && detFork.fd != 0 {
			detFork.stop = detP(pp).switches + 1
			detFeed.novelStop = true
		}
	}
}

// detFeedSite hashes the depth innermost return addresses above frame fp.
//
//go:nosplit
func detFeedSite(fp uintptr, depth int) uint64 {
	if goarch.ArchFamily != goarch.AMD64 && goarch.ArchFamily != goarch.ARM64 {
		return 0
	}
	h := uint64(depth)
	for i := 0; i < depth && fp != 0; i++ {
		pc := *(*uintptr)(unsafe.Pointer(fp + goarch.PtrSize))
		h = (h ^ uint64(pc-firstmoduledata.text)) * 0x9e3779b97f4a7c15
		h ^= h >> 29
		fp = *(*uintptr)(unsafe.Pointer(fp))
	}
	return h * 0xbf58476d1ce4e5b9
}

// detFeedMix combines two hashes into a feature hash.
//
//go:nosplit
func detFeedMix(a, b uint64) uint64 {
	h := (a ^ b*0x94d049bb133111eb) * 0x9e3779b97f4a7c15
	return h ^ h>>31
}

// The hooks below are called from upstream code. Each is a guard that
// inlines into its caller and folds away without the experiment, and a
// function that records.

// detFeedParked is called by gopark.
//
//go:nosplit
func detFeedParked(reason waitReason) {
	if goexperiment.DetSched && detFeed.on {
		detFeedParked1(reason)
	}
}

//go:nosplit
func detFeedParked1(reason waitReason) {
	detFeedHit(detFeedPark, detFeedMix(detFeedSite(getfp(), 10), uint64(reason)))
}

// detFeedSelected is called by selectgo with the case it chose, or -1.
// Cases below nsends are sends.
func detFeedSelected(scases []scase, casi, nsends int) {
	if goexperiment.DetSched && detFeed.on {
		detFeedSelected1(scases, casi, nsends)
	}
}

func detFeedSelected1(scases []scase, casi, nsends int) {
	site := detFeedSite(getfp(), 3)
	detFeedHit(detFeedSelect, detFeedMix(site, uint64(casi+1)))
	if casi < 0 {
		return
	}
	if c := scases[casi].c; casi < nsends {
		detFeedSent(c, site)
	} else {
		detFeedReceived(c, site)
	}
}

// detFeedSend is called by chansend1 before it sends on c.
//
//go:nosplit
func detFeedSend(c *hchan) {
	if goexperiment.DetSched && detFeed.on && c != nil {
		detFeedSend1(c)
	}
}

//go:nosplit
func detFeedSend1(c *hchan) {
	detFeedSent(c, detFeedSite(getfp(), 3))
}

// detFeedRecv is called by chanrecv1 and chanrecv2 after a receive from c.
//
//go:nosplit
func detFeedRecv(c *hchan) {
	if goexperiment.DetSched && detFeed.on && c != nil {
		detFeedRecv1(c)
	}
}

//go:nosplit
func detFeedRecv1(c *hchan) {
	detFeedReceived(c, detFeedSite(getfp(), 3))
}

//go:nosplit
func detFeedChanSlot(c *hchan) uintptr {
	return uintptr(uint64(uintptr(unsafe.Pointer(c))>>4)*0x9e3779b97f4a7c15>>52) & (uintptr(len(detFeed.lastSend)) - 1)
}

//go:nosplit
func detFeedSent(c *hchan, site uint64) {
	detFeed.lastSend[detFeedChanSlot(c)] = site
	if c.dataqsiz > 0 {
		detFeedHit(detFeedMax, detFeedMix(site, uint64(bits.Len(c.qcount))))
	}
}

//go:nosplit
func detFeedReceived(c *hchan, site uint64) {
	detFeedHit(detFeedComm, detFeedMix(detFeed.lastSend[detFeedChanSlot(c)], site))
}

// detFeedRecovered is called by gorecover when it recovers a panic.
//
//go:nosplit
func detFeedRecovered() {
	if goexperiment.DetSched && detFeed.on {
		detFeedRecovered1()
	}
}

//go:nosplit
func detFeedRecovered1() {
	detFeedHit(detFeedRecover, detFeedSite(getfp(), 3))
}

// Keys for detFeedMaxOf.
const (
	detFeedMaxGoroutines = 1 + iota
	detFeedMaxTime
)

// detFeedMaxOf records value v of what, in power-of-two buckets.
//
//go:nosplit
func detFeedMaxOf(what int, v uint64) {
	if detFeed.on {
		detFeedHit(detFeedMax, detFeedMix(uint64(what), uint64(bits.Len64(v))))
	}
}

//go:linkname internal_sync_runtime_detLocked internal/sync.runtime_detLocked
//go:nosplit
func internal_sync_runtime_detLocked(l unsafe.Pointer) {
	// A simulation needs the held mutexes too; see detsimSemacquire.
	if !detFeed.on && detsim.handler == nil {
		return
	}
	gp := getg()
	s := detG(gp)
	s.woke = 0
	if s.heldGoid != gp.goid {
		s.heldGoid, s.nheld = gp.goid, 0
	}
	var pcs [detFeedLockFrames]uintptr
	if goarch.ArchFamily == goarch.AMD64 || goarch.ArchFamily == goarch.ARM64 {
		fp := getfp()
		for i := 0; i < len(pcs) && fp != 0; i++ {
			pcs[i] = *(*uintptr)(unsafe.Pointer(fp + goarch.PtrSize))
			fp = *(*uintptr)(unsafe.Pointer(fp))
		}
	}
	for i := 0; i < int(s.nheld) && detFeed.on; i++ {
		detFeedLockEdgeOf(&s.held[i], uintptr(l), &pcs, gp.goid)
	}
	if int(s.nheld) == len(s.held) {
		copy(s.held[:], s.held[1:])
		s.nheld--
	}
	s.held[s.nheld] = detHeldLock{uintptr(l), detHeldKeep(gp, l), pcs}
	s.nheld++
	if detFeed.on && detFeed.nyield > 0 {
		h := detFeedPCsHash(&pcs)
		for _, y := range detFeed.yield[:detFeed.nyield] {
			if y == h {
				detFeedHold()
				break
			}
		}
	}
}

// detFeedHold lets other goroutines run while the running one holds the
// lock it just acquired, so that one of them may take a lock in the
// opposite order (DeadlockFuzzer). It does so with detFeed.hold's chance,
// which is 16/16 without perturbation and otherwise drawn, the first time,
// from 16, 16, 8, and 4: holding at every acquisition can
// delay the program past the one where the cycle can close, for example
// when it can close only on a later iteration. Under PCT the goroutine drops below
// every other, so it runs again only when they are blocked or demoted;
// otherwise it yields while another goroutine is runnable, at most
// detFeedHoldYields times. Whether another is runnable depends only on
// the schedule so far, so the schedule stays deterministic.
func detFeedHold() {
	gp := getg()
	pp := gp.m.p.ptr()
	if detFeed.hold == 0 {
		detFeed.hold = 16
		if detPerturbStrategy != 0 {
			detFeed.hold = [...]uint32{16, 16, 8, 4}[detSchedRand(pp)%4]
		}
	}
	if detFeed.hold < 16 && !detChance(pp, detFeed.hold) {
		return
	}
	if detPerturb.pct {
		s := detPCTState(pp, gp)
		s.prio, s.used = detPCT.lowest, 0
		detPCT.lowest--
		mcall(gosched_m)
		return
	}
	for range detFeedHoldYields {
		if runqempty(pp) && sched.runq.size == 0 {
			return
		}
		mcall(gosched_m)
	}
}

//go:linkname internal_sync_runtime_detUnlocked internal/sync.runtime_detUnlocked
//go:nosplit
func internal_sync_runtime_detUnlocked(l unsafe.Pointer) {
	gp := getg()
	s := detG(gp)
	if s.heldGoid == gp.goid && (detFeed.on || detsim.handler != nil) {
		// A mutex may be unlocked by another goroutine; then its holder
		// keeps a stale entry until it overflows.
		for i := int(s.nheld) - 1; i >= 0; i-- {
			if s.held[i].l == uintptr(l) {
				copy(s.held[i:], s.held[i+1:s.nheld])
				s.nheld--
				s.held[s.nheld].keep = nil
				break
			}
		}
	}
	if s.zombie == uintptr(l) {
		// A zombie released the mutex it ran on for (see
		// detsimSemacquire): freeze it now, before a loop takes the
		// mutex again; the scheduler freezes it (detsimFreeze). This
		// holds also once the simulation closed.
		s.zombie = 0
		if gp.m.locks == 0 && gp == gp.m.curg {
			mcall(gosched_m)
		} else {
			gp.preempt = true
			gp.stackguard0 = stackPreempt
		}
	}
}

//go:linkname sync_runtime_detRLocked sync.runtime_detRLocked
//go:nosplit
func sync_runtime_detRLocked(w unsafe.Pointer) {
	// Only a simulation needs readers (see detsimSemacquire), and
	// feedback leaves them out, so lock edges and holds stay as they were.
	if detsim.handler == nil {
		return
	}
	gp := getg()
	s := detG(gp)
	if s.heldGoid != gp.goid {
		s.heldGoid, s.nheld = gp.goid, 0
	}
	if int(s.nheld) == len(s.held) {
		copy(s.held[:], s.held[1:])
		s.nheld--
	}
	s.held[s.nheld] = detHeldLock{l: uintptr(w), keep: detHeldKeep(gp, w)}
	s.nheld++
}

//go:linkname sync_runtime_detRUnlocked sync.runtime_detRUnlocked
//go:nosplit
func sync_runtime_detRUnlocked(w unsafe.Pointer) {
	internal_sync_runtime_detUnlocked(w)
}

//go:nosplit
func detFeedPCsHash(pcs *[detFeedLockFrames]uintptr) uint64 {
	h := uint64(0)
	for _, pc := range pcs {
		h = (h ^ uint64(pc-firstmoduledata.text)) * 0x9e3779b97f4a7c15
		h ^= h >> 29
	}
	return h
}

// detFeedLockEdgeOf records that goroutine goid acquired lock l, at pcs,
// while holding h.
//
//go:nosplit
func detFeedLockEdgeOf(h *detHeldLock, l uintptr, pcs *[detFeedLockFrames]uintptr, goid uint64) {
	if h.l == l {
		return
	}
	detFeedHit(detFeedLockEdge, detFeedMix(detFeedPCsHash(&h.pcs), detFeedPCsHash(pcs)))
	// Keep each (held lock, new lock) once, in an open addressing table,
	// for predicting deadlocks at exit.
	e := detFeedEdgeLookup(h.l, l)
	switch {
	case e == nil:
	case e.a == 0:
		if detFeed.nedges < len(detFeed.edges)*3/4 {
			*e = detLockEdge{a: h.l, b: l, goid: goid, pa: h.pcs, pb: *pcs}
			detFeed.nedges++
		}
	case e.goid != goid:
		e.multi = true
	}
}

// detFeedEdgeLookup returns the entry for edge (a, b), or the empty entry
// where it would go, or nil if the table is full.
//
//go:nosplit
func detFeedEdgeLookup(a, b uintptr) *detLockEdge {
	n := uintptr(len(detFeed.edges))
	i := uintptr(detFeedMix(uint64(a), uint64(b))>>32) & (n - 1)
	for range n {
		e := &detFeed.edges[i]
		if e.a == 0 || e.a == a && e.b == b {
			return e
		}
		i = (i + 1) & (n - 1)
	}
	return nil
}

// SDK assertions.
//
// detsched/sdk/assert calls detSDKAssert for each assertion it evaluates.
// The kind and message identify the assertion, as in the Antithesis SDK.
// An assertion counts as a feedback feature, and a failure (an Always
// that is false, an Unreachable that is reached) is reported at exit and
// makes a run that would exit with status 0 exit with status 3. The
// report gives each failing assertion once, with the details of its first
// failure, cut to 512 bytes, and how many more times it failed, up to
// 64 KiB in all, and then how many more failures there were. The failures
// are kept in fixed buffers, so recording them does not change the
// schedule.

// Kinds of assertion, the values detsched/sdk/assert passes.
const (
	detAssertAlways = iota
	detAssertAlwaysOrUnreachable
	detAssertSometimes
	detAssertReachable
	detAssertUnreachable
	detAssertKinds
)

var detAssertNames = [detAssertKinds]string{"always", "always-or-unreachable", "sometimes", "reachable", "unreachable"}

// A detAssertion counts the evaluations of one assertion.
type detAssertion struct {
	id            uint64
	kind          uint8
	n             uint8 // bytes of msg
	trues, falses uint32
	msg           [120]byte
}

var detSDK struct {
	asserts  [256]detAssertion
	failures int
	base     int // in a branch, the failures of the timeline before it forked

	// Each failing assertion's message and first details, cut to 512
	// bytes, after a header of detAllHeader bytes: their length and the
	// message's in two bytes each, then how many more times it failed in
	// four. allCount is the failures they hold, repeats included.
	all      [64 << 10]byte
	allN     int
	allCount int
}

const detAllHeader = 8

// detSDKFailure writes msg and details, separated by a space, to b, as
// much as fits, and returns the length written.
func detSDKFailure(b []byte, msg, details string) int {
	k := copy(b, msg)
	if details != "" && k < len(b) {
		b[k] = ' '
		k++
		k += copy(b[k:], details)
	}
	return k
}

// detAssertFailed records a failing assertion with message msg and
// details. Every failure passes through it, so a debugger can stop at
// failures with a breakpoint on it.
//
//go:noinline
func detAssertFailed(msg, details string) {
	detSDK.failures++
	for i := 0; i < detSDK.allN; {
		k := int(detSDK.all[i]) | int(detSDK.all[i+1])<<8
		m := int(detSDK.all[i+2]) | int(detSDK.all[i+3])<<8
		if m == min(len(msg), 512) && string(detSDK.all[i+detAllHeader:i+detAllHeader+m]) == msg[:m] {
			c := detSDK.all[i+4 : i+8]
			n := (uint32(c[0]) | uint32(c[1])<<8 | uint32(c[2])<<16 | uint32(c[3])<<24) + 1
			c[0], c[1], c[2], c[3] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
			detSDK.allCount++
			return
		}
		i += detAllHeader + k
	}
	// A failure that does not fit whole is only counted, so that no
	// message is cut short but by the 512-byte limit.
	need := min(len(msg)+1+len(details), 512)
	if detSDK.allN+detAllHeader+need > len(detSDK.all) {
		return
	}
	b := detSDK.all[detSDK.allN:]
	k := detSDKFailure(b[detAllHeader:detAllHeader+need], msg, details)
	m := min(len(msg), 512)
	b[0], b[1], b[2], b[3] = byte(k), byte(k>>8), byte(m), byte(m>>8)
	b[4], b[5], b[6], b[7] = 0, 0, 0, 0
	detSDK.allN += detAllHeader + k
	detSDK.allCount++
}

// testing_detAssertFailures returns how many assertions have failed, in a
// branch since it forked, so that package testing can fail a test during
// which one failed, and so print its log.
//
//go:linkname testing_detAssertFailures testing.detAssertFailures
func testing_detAssertFailures() int { return detSDK.failures - detSDK.base }

// detSDKBranch starts a branch's record of failures afresh, so that it
// reports only the assertions that fail in it (see detsimBranch).
func detSDKBranch() {
	detSDK.base = detSDK.failures
	detSDK.allN, detSDK.allCount = 0, 0
}

// detSDKAssert records an assertion of kind with message msg evaluated to
// cond. details describes a failing assertion; it is empty otherwise.
//
//go:linkname detSDKAssert github.com/ryanworl/go-detsched/misc/detsched/sdk/assert.runtime_assert
func detSDKAssert(kind int, cond bool, msg, details string) {
	if kind < 0 || kind >= detAssertKinds {
		return
	}
	id := uint64(kind) + 1
	for i := 0; i < len(msg); i++ {
		id = (id ^ uint64(msg[i])) * 0x100000001b3
	}
	failed := false
	switch kind {
	case detAssertAlways, detAssertAlwaysOrUnreachable:
		failed = !cond
	case detAssertUnreachable:
		failed = true
	}
	if failed {
		// Before the feature, so that the failure is not new (see
		// detFeedHit).
		detAssertFailed(msg, details)
	}

	b := uint64(0)
	if cond {
		b = 1
	}
	detFeedHit(detFeedAssert, detFeedMix(id, b))

	n := uint64(len(detSDK.asserts))
	for i, probes := id&(n-1), uint64(0); probes < n; i, probes = (i+1)&(n-1), probes+1 {
		a := &detSDK.asserts[i]
		if a.id == 0 {
			a.id, a.kind = id, uint8(kind)
			a.n = uint8(copy(a.msg[:], msg))
		} else if a.id != id {
			continue
		}
		if cond {
			a.trues++
		} else {
			a.falses++
		}
		break
	}
}

// detSDKReport prints the assertion failures, and with feedback every
// assertion's counts. Called at exit.
func detSDKReport() {
	for i := 0; i < detSDK.allN; {
		k := int(detSDK.all[i]) | int(detSDK.all[i+1])<<8
		c := detSDK.all[i+4 : i+8]
		more := uint32(c[0]) | uint32(c[1])<<8 | uint32(c[2])<<16 | uint32(c[3])<<24
		f := ""
		if k > 0 {
			f = slicebytetostringtmp(&detSDK.all[i+detAllHeader], k)
		}
		switch more {
		case 0:
			print("detsched: assertion failed: ", f, "\n")
		case 1:
			print("detsched: assertion failed: ", f, " (and 1 more failure)\n")
		default:
			print("detsched: assertion failed: ", f, " (and ", more, " more failures)\n")
		}
		i += detAllHeader + k
	}
	if n := detSDK.failures - detSDK.base - detSDK.allCount; n > 0 {
		print("detsched: and ", n, " more assertion failures\n")
	}
	if !detFeed.on {
		return
	}
	for i := range detSDK.asserts {
		a := &detSDK.asserts[i]
		if a.id != 0 {
			print("detsched: assert ", detAssertNames[a.kind], " true=", a.trues, " false=", a.falses, " ", slicebytetostringtmp(&a.msg[0], int(a.n)), "\n")
		}
	}
}

// detFeedReport prints a summary of the features and the deadlocks the
// lock order predicts, and writes the counters to the file, if any. A
// fork-mode child given a feedback descriptor writes the counters there,
// followed by four uint64s: the exit status, the first novel feature + 1
// (or 0), the switch it was recorded in, and the number of switches.
func detFeedReport(code int) {
	if !detFeed.on {
		return
	}
	if detCover.n > 0 {
		detFeedCoverScan()
	}
	if detFeed.out > 0 {
		var n uint64
		for _, pp := range allp {
			n += detP(pp).switches
		}
		trailer := [4]uint64{uint64(code), detFeed.novel, detFeed.novelSwitch, n}
		write1(uintptr(detFeed.out), unsafe.Pointer(&detFeed.hits[0]), int32(len(detFeed.hits)))
		write1(uintptr(detFeed.out), unsafe.Pointer(&trailer), int32(unsafe.Sizeof(trailer)))
		closefd(detFeed.out)
		detFeed.out = 0
	}
	var h uint64
	total := 0
	print("detsched: feedback")
	for k := 0; k < detFeedKinds; k++ {
		n := 0
		end := (k + 1) << detFeedBits
		if k == detFeedGuide {
			end = detFeedLen
		}
		for i, c := range detFeed.hits[k<<detFeedBits : end] {
			if c != 0 {
				n++
				// Hash the AFL bucket of the count, not the count.
				h = detFeedMix(h, uint64(k<<detFeedBits+i)<<8|uint64(bits.Len8(c)))
			}
		}
		total += n
		print(" ", detFeedNames[k], "=", n)
	}
	print(" total=", total, " hash=", hex(h), " lockpairs=", detFeed.nedges, "\n")

	// A lock order cycle of two locks taken in opposite orders by two
	// goroutines predicts a deadlock (Goodlock), unless a third lock
	// guards both, or the address was reused for another lock after a
	// GC; this prototype checks neither.
	cycles := 0
	for i := range detFeed.edges {
		e := &detFeed.edges[i]
		if e.a == 0 || e.a > e.b {
			continue
		}
		if r := detFeedEdgeLookup(e.b, e.a); r != nil && r.a != 0 {
			if e.multi || r.multi || r.goid != e.goid {
				if cycles < 4 {
					print("detsched: potential deadlock: goroutine ", e.goid, " locked ", hex(e.a), " at ")
					detFeedPrintSite(&e.pa)
					print(" then ", hex(e.b), " at ")
					detFeedPrintSite(&e.pb)
					print("; goroutine ", r.goid, " locked them in the other order at ")
					detFeedPrintSite(&r.pa)
					print(" and ")
					detFeedPrintSite(&r.pb)
					print("\n")
					print("detsched: deadlock directive yield=", hex(detFeedPCsHash(&e.pa)), ",", hex(detFeedPCsHash(&r.pa)), "\n")
				}
				cycles++
			}
		}
	}
	if cycles > 0 {
		print("detsched: ", cycles, " potential deadlocks\n")
	}
	if detFeed.hold != 0 {
		print("detsched: deadlock hold chance ", detFeed.hold, "/16\n")
	}

	if detFeed.path[0] != 0 {
		fd := open(&detFeed.path[0], 0x241, 0o644) // O_WRONLY|O_CREAT|O_TRUNC
		if fd >= 0 {
			write1(uintptr(fd), unsafe.Pointer(&detFeed.hits[0]), int32(len(detFeed.hits)))
			closefd(fd)
		}
	}
}

// detFeedPrintSite prints the innermost frame of pcs, counting inlined
// frames, outside the runtime and the sync packages.
func detFeedPrintSite(pcs *[detFeedLockFrames]uintptr) {
	for _, pc := range pcs {
		f := findfunc(pc)
		if !f.valid() {
			continue
		}
		u, uf := newInlineUnwinder(f, pc-1)
		for ; uf.valid(); uf = u.next(uf) {
			name := u.srcFunc(uf).name()
			if stringslite.HasPrefix(name, "runtime.") || stringslite.HasPrefix(name, "sync.") || stringslite.HasPrefix(name, "internal/sync.") {
				continue
			}
			file, line := u.fileLine(uf)
			print(name, " (", file, ":", line, ")")
			return
		}
	}
	print("?")
}

// Guidance.
//
// detsched/sdk/guide lets the program describe its progress to a search,
// as FuzzFactory's waypoints do: State reports an abstract state, a
// feature of its own, and Maximize a value the search should push up,
// which records one feature per bucket up to the value's, so that only a
// value in a bucket above every earlier one is new.

func detGuideName(name string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := 0; i < len(name); i++ {
		h = (h ^ uint64(name[i])) * 0x100000001b3
	}
	return h
}

//go:linkname detSDKState github.com/ryanworl/go-detsched/misc/detsched/sdk/guide.runtime_state
func detSDKState(name string, state uint64) {
	detFeedHit(detFeedGuide, detFeedMix(detGuideName(name), state))
}

//go:linkname detSDKMaximize github.com/ryanworl/go-detsched/misc/detsched/sdk/guide.runtime_maximize
func detSDKMaximize(name string, v int64) {
	// Buckets: each value below 32, then each power of two.
	b := 0
	switch {
	case v >= 32:
		b = 27 + bits.Len64(uint64(v))
	case v > 0:
		b = int(v)
	}
	h := detGuideName(name) ^ 0x5bd1e995
	for i := 0; i <= b; i++ {
		detFeedHit(detFeedGuide, detFeedMix(h, uint64(i)))
	}
}
