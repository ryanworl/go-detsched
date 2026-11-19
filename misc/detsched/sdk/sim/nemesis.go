// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

// A Nemesis injects faults into a world, after Antithesis's fault
// injector: network partitions, clogs, and single links cut (Stopped,
// Slowed, or Jammed), restores, node kills, process kills, graceful stops, pauses, and
// throttles, and clock skips, each for a random duration. All of its
// choices come from the fault stream, so a branched run explores other
// faults.
//
// Faults come in storms separated by calms. A calm ends every fault,
// restarting the nodes the nemesis took down, and turns the simulation's
// buggify faults off, as StopFaults does; a storm injects faults. Each run
// draws its shape: one run in 16 has no faults at all, one in 4 of the
// rest has no calms, one storm for as long as the nemesis runs, and the
// others draw the longest storm (5s to 2m) and the longest calm (2s to
// 30s) they have, so that some are mostly storms and some mostly calms.
// Each storm then draws its own length (from 2s), how often faults start
// (every 50ms to 5s on average), how long they last (100ms to 20s on
// average), and a random subset of the fault kinds and modes that
// NemesisConfig allows (swarm testing), and one storm in three aims its
// node faults, clogs, and links at one node; each calm draws its length
// (from 1s). Faults overlap as the gap and duration make them, except that
// at most a minority of the nodes (at least 1) is down, paused, or
// throttled by the nemesis at once. All lengths are
// log-uniform, so storms range from bursts of short flaps to long
// partitions. A run starts with a storm or a calm, and each branch point
// of a search draws the shape and the current storm or calm again. A
// process kill is three times as likely as any other kind.
//
// The nemesis reports each action as an assertion (assert.Reachable, such
// as "nemesis: clog jammed"), in the fault timeline that a failing run
// prints, and to NemesisConfig.Event, through which a harness can track
// faults against its own state, such as whether the node killed was the
// leader.
type Nemesis struct {
	w   *World
	cfg NemesisConfig

	req  chan nemesisReq
	done chan struct{}

	epoch  uint64         // of the current plan
	kinds  []string       // fault kinds enabled in this storm
	modes  []Mode         // network modes enabled in this storm
	active []*activeFault // faults to end, by end time
	down   map[*Node]bool // nodes the nemesis took down or paused

	storm    bool          // in a storm; otherwise in a calm
	calls    uint32        // harness callbacks begun and ended; see nemesisNode
	phaseEnd time.Time     // when the storm or calm ends
	minCalm  time.Time     // StopFaults: no storm before then
	next     time.Time     // the storm's next fault
	gap, dur time.Duration // the storm's mean gap and fault duration
	focus    *Node         // the node the storm aims at, or nil

	noCalm            bool                      // the run is one storm
	restarting        int32                     // 1 + the index of the node Restart restarts; see calls
	stormMax, calmMax time.Duration             // the run's longest storm and calm
	calmSince         atomic.Pointer[time.Time] // the calm's start; nil in a storm
}

// Fault kinds, for NemesisConfig.Disable and Event.Kind.
const (
	FaultPartition   = "partition"   // the nodes split into groups
	FaultClog        = "clog"        // one node's traffic in, out, or both
	FaultLink        = "link"        // one link, in one direction or both
	FaultRestore     = "restore"     // every network fault ends early
	FaultKill        = "kill"        // power failure, then restart
	FaultKillProcess = "killprocess" // process crash, then restart
	FaultStop        = "stop"        // graceful stop, then restart
	FaultPause       = "pause"       // pause, then resume
	FaultThrottle    = "throttle"    // throttle, then unthrottle
	FaultClockSkip   = "clockskip"   // the wall clock jumps
	FaultDisk        = "disk"        // one node's syncs slow down or stall

	// FaultClockSkipBack, in NemesisConfig.Disable, keeps clock skips
	// from moving the wall clock back, for a system whose known bugs
	// every backward step would find again. Its skips are reported as
	// FaultClockSkip.
	FaultClockSkipBack = "clockskipback"
)

// Phases, for Event.Kind. Their events have Start set and no end event:
// a storm ends where a calm starts, and the other way around.
const (
	PhaseStorm = "storm"
	PhaseCalm  = "calm"
)

var allFaults = []string{FaultPartition, FaultClog, FaultLink, FaultRestore, FaultKill, FaultKillProcess, FaultStop, FaultPause, FaultThrottle, FaultClockSkip, FaultDisk}

// The ranges a run's shape, its storms, and its calms draw from,
// log-uniformly.
const (
	stormMin, calmMin      = 2 * time.Second, time.Second
	stormMaxLo, stormMaxHi = 5 * time.Second, 2 * time.Minute
	calmMaxLo, calmMaxHi   = 2 * time.Second, 30 * time.Second
	gapMin, gapMax         = 50 * time.Millisecond, 5 * time.Second
	durMin, durMax         = 100 * time.Millisecond, 20 * time.Second
)

// NemesisConfig configures a Nemesis.
type NemesisConfig struct {
	// Nodes are the nodes to fault (default: the world's nodes at the
	// time of each fault, without removed ones).
	Nodes []*Node
	// Exclude lists nodes the nemesis never kills, stops, pauses,
	// throttles, or slows the disk of, such as the clients of a workload
	// or a coordinator that cannot restart. Network faults still reach
	// them: a partition puts each in a random group, and clogs and links
	// pick them like the other nodes, so that a client can reach some
	// servers but not others.
	Exclude []*Node
	// Restart starts a node again after a kill or stop. It is called from
	// the nemesis's goroutine, which belongs to no node. Without it, the
	// nemesis terminates no nodes. Like Event, it must return promptly.
	Restart func(*Node)
	// Disable lists fault kinds never to inject, such as FaultClockSkip
	// for a system that does not claim to survive clock jumps.
	Disable []string
	// Event, if set, is called on the nemesis's goroutine after a fault
	// starts and after it ends, and when a storm or a calm starts. A
	// FaultStop starts once Node.Stop has returned. The simulation's own
	// faults, buggify sites such as connection resets and disk errors,
	// come during storms too, without events.
	//
	// Event and Restart run on the goroutine that resumes the nodes the
	// nemesis paused, so they must not wait for those nodes: they must
	// not take locks that node goroutines hold, such as a mutex that the
	// harness's log or state shares with code running on the nodes, nor
	// wait for a node's goroutine in any other way. A callback that did
	// would wait forever while the other nodes' timers move the clock on.
	// The runtime fails the program instead: at once when a callback is
	// about to wait for a sync.Mutex or RWMutex that a goroutine of a node
	// the nemesis paused holds, and when a callback has been blocked for
	// a minute of the bubble's time.
	Event func(Event)
}

// Event is a nemesis action.
type Event struct {
	Kind  string        // one of the Fault or Phase constants
	Start bool          // the fault or phase started; false: the fault ended
	Desc  string        // such as "clog jammed n2 in"
	Node  *Node         // the node of a node fault or clog
	Net   *NetFault     // the network fault, for partitions and clogs
	Skip  time.Duration // a clock skip's size; its end (Start false) steps the clock by -Skip
}

type activeFault struct {
	end  time.Time
	ev   Event
	undo func()
}

type nemesisReq struct {
	quiet time.Duration // pause faults for this long
	stop  bool          // end every fault and exit
	done  chan struct{}
}

// NewNemesis creates a nemesis for w. Start runs it.
func (w *World) NewNemesis(cfg NemesisConfig) *Nemesis {
	return &Nemesis{w: w, cfg: cfg, req: make(chan nemesisReq), done: make(chan struct{}), down: map[*Node]bool{}}
}

// Start runs the nemesis in a goroutine that belongs to no node.
func (nm *Nemesis) Start() {
	nm.w.endSetup()
	old := runtime_setNode(0)
	go nm.loop()
	runtime_setNode(old)
}

// StopFaults starts a calm that lasts at least d, as ANTITHESIS_STOP_FAULTS
// does: it ends every fault the nemesis injected, restarting nodes it took
// down, and turns off the simulation's buggify faults, such as
// retransmission delays and disk errors, after which storms go on. A calm
// already running is extended. Use it before checking that a system
// recovers. It returns once the faults have ended. Clock skips stay.
func (nm *Nemesis) StopFaults(d time.Duration) {
	r := nemesisReq{quiet: d, done: make(chan struct{})}
	nm.req <- r
	<-r.done
}

// Stop ends every fault the nemesis injected, as StopFaults does, and stops
// the nemesis. The simulation's buggify faults stay off from then on.
func (nm *Nemesis) Stop() {
	r := nemesisReq{stop: true, done: make(chan struct{})}
	nm.req <- r
	<-r.done
	<-nm.done
}

// CalmSince reports whether the nemesis is in a calm, with every fault it
// injected ended and the simulation's buggify faults off, and since when.
// It is false before Start and during a storm, including between its
// faults, as buggify faults come then too. Use it, rather than tracking
// Events, to tell whether a check that needs a healthy cluster applies:
// it is safe on any goroutine, and it turns true only after the faults
// have ended and false before the first fault of a storm starts.
func (nm *Nemesis) CalmSince() (time.Time, bool) {
	if t := nm.calmSince.Load(); t != nil {
		return *t, true
	}
	return time.Time{}, false
}

func (nm *Nemesis) loop() {
	defer close(nm.done)
	runtime_watch(&nm.calls, nemesisNode)
	nm.epoch = runtime_epoch()
	nm.plan(time.Now())
	for {
		wake := nm.phaseEnd
		if nm.storm && nm.next.Before(wake) {
			wake = nm.next
		}
		if len(nm.active) > 0 && nm.active[0].end.Before(wake) {
			wake = nm.active[0].end
		}
		select {
		case r := <-nm.req:
			now := time.Now()
			if r.stop {
				nm.startCalm(now, forever(now))
				runtime_watch(nil, nil)
				close(r.done)
				return
			}
			end := now.Add(r.quiet)
			if !nm.storm && nm.phaseEnd.After(end) {
				end = nm.phaseEnd
			}
			nm.minCalm = end
			nm.startCalm(now, end)
			close(r.done)
			continue
		case <-time.After(time.Until(wake)):
		}
		if world != nm.w {
			// It would restart nodes that then use the real OS.
			panic("sim: the World of a running Nemesis was closed; stop the Nemesis first")
		}
		now := time.Now()
		for len(nm.active) > 0 && !nm.active[0].end.After(now) {
			a := nm.active[0]
			nm.active = nm.active[1:]
			nm.end(a)
		}
		if e := runtime_epoch(); e != nm.epoch {
			nm.epoch = e
			nm.plan(now)
			continue
		}
		if !nm.phaseEnd.After(now) {
			if nm.storm {
				nm.startCalm(now, now.Add(logUniform(calmMin, nm.calmMax)))
			} else {
				nm.startStorm(now)
			}
			continue
		}
		if nm.storm && !nm.next.After(now) {
			nm.inject()
			nm.next = now.Add(expRand(nm.gap))
		}
	}
}

// forever is a time no run reaches.
func forever(now time.Time) time.Time { return now.Add(1 << 62) }

// plan starts the run, or, at a branch point, starts it over from now: it
// draws the run's shape, and starts a storm or a calm, or a run without
// faults.
func (nm *Nemesis) plan(now time.Time) {
	nm.noCalm = Intn(4) == 0
	nm.stormMax = logUniform(stormMaxLo, stormMaxHi)
	nm.calmMax = logUniform(calmMaxLo, calmMaxHi)
	switch {
	case Intn(16) == 0:
		assert.Reachable("nemesis: a run without faults", nil)
		nm.startCalm(now, forever(now))
	case !nm.noCalm && Intn(2) == 0 || now.Before(nm.minCalm):
		end := now.Add(logUniform(calmMin, nm.calmMax))
		if end.Before(nm.minCalm) {
			end = nm.minCalm
		}
		nm.startCalm(now, end)
	default:
		nm.startStorm(now)
	}
}

// startStorm starts a storm, drawing its length and intensity.
func (nm *Nemesis) startStorm(now time.Time) {
	if !nm.storm {
		assert.Reachable("nemesis: a storm starts", nil)
	}
	nm.calmSince.Store(nil)
	nm.storm = true
	nm.phaseEnd = now.Add(logUniform(stormMin, nm.stormMax))
	if nm.noCalm {
		assert.Reachable("nemesis: a run that is one storm", nil)
		nm.phaseEnd = forever(now)
	}
	nm.gap = logUniform(gapMin, gapMax)
	nm.dur = logUniform(durMin, durMax)
	nm.focus = nil
	if t := nm.nodeTargets(); len(t) > 0 && Intn(3) == 0 {
		nm.focus = t[Intn(len(t))]
	}
	nm.swarm()
	nm.next = now.Add(expRand(nm.gap))
	var b strings.Builder
	length := nm.phaseEnd.Sub(now).Round(time.Millisecond).String()
	if nm.noCalm {
		length = "to the end"
	}
	fmt.Fprintf(&b, "storm %s: a fault every %v for %v, %s", length, nm.gap.Round(time.Millisecond), nm.dur.Round(time.Millisecond), strings.Join(nm.kinds, ","))
	if nm.focus != nil {
		fmt.Fprintf(&b, ", aimed at %s", nm.focus.name)
	}
	nm.event(Event{Kind: PhaseStorm, Start: true, Desc: b.String()})
}

// startCalm ends every fault and starts a calm that lasts until end.
func (nm *Nemesis) startCalm(t, end time.Time) {
	if nm.storm {
		assert.Reachable("nemesis: a calm after a storm", nil)
	}
	nm.storm = false
	nm.phaseEnd = end
	nm.endAll()
	quiet := int64(1<<63 - 1)
	if d := end.Sub(t); d < 1<<61 {
		quiet = now() + int64(d)
	}
	runtime_call(func() {
		nm.w.quiet = quiet
		for _, n := range nm.w.nodes {
			n.disk.endFaults()
		}
	})
	for _, n := range nm.w.nodes {
		n.wakeDisk() // ends "sim.disk.slowsync" stalls
	}
	desc := "calm"
	if quiet != 1<<63-1 {
		desc = "calm " + end.Sub(t).Round(time.Millisecond).String()
	}
	if nm.calmSince.Load() == nil {
		nm.calmSince.Store(&t)
	}
	nm.event(Event{Kind: PhaseCalm, Start: true, Desc: desc})
}

// logUniform returns a duration between lo and hi whose logarithm is
// uniformly distributed, from the fault stream.
func logUniform(lo, hi time.Duration) time.Duration {
	u := float64(runtime_rand()&(1<<53-1)) / (1 << 53)
	return time.Duration(float64(lo) * math.Exp(u*math.Log(float64(hi)/float64(lo))))
}

// netTargets returns the nodes the nemesis's network faults may reach
// now.
func (nm *Nemesis) netTargets() []*Node {
	nodes := nm.cfg.Nodes
	if nodes == nil {
		nodes = nm.w.nodes
	}
	return slices.DeleteFunc(slices.Clone(nodes), func(n *Node) bool { return n.removed })
}

// nodeTargets returns the nodes the nemesis's node faults may hit now.
func (nm *Nemesis) nodeTargets() []*Node {
	return slices.DeleteFunc(nm.netTargets(), func(n *Node) bool { return slices.Contains(nm.cfg.Exclude, n) })
}

// swarm picks the fault kinds and modes of a storm.
func (nm *Nemesis) swarm() {
	var allowed []string
	for _, k := range allFaults {
		if slices.Contains(nm.cfg.Disable, k) {
			continue
		}
		switch k {
		case FaultKill, FaultKillProcess, FaultStop:
			if nm.cfg.Restart == nil {
				continue
			}
		}
		allowed = append(allowed, k)
	}
	runtime_call(nm.reportKinds) // on the system stack, which counts no ticks
	nm.kinds = pickSome(allowed)
	nm.modes = pickSome([]Mode{Slowed, Jammed, Stopped})
}

// reportKinds reports each fault kind a storm could pick, and each that
// the configuration keeps out, with why, for the detsched command's
// warning of faults no run injected.
func (nm *Nemesis) reportKinds() {
	net, nodes := len(nm.netTargets()), len(nm.nodeTargets())
	for _, k := range allFaults {
		if k == FaultRestore || slices.Contains(nm.cfg.Disable, k) {
			continue
		}
		why := ""
		switch k {
		case FaultKill, FaultKillProcess, FaultStop:
			if nm.cfg.Restart == nil {
				why = "NemesisConfig.Restart is nil"
				break
			}
			fallthrough
		case FaultPause, FaultThrottle, FaultDisk:
			if nodes == 0 {
				why = "NemesisConfig.Exclude has every node"
			}
		case FaultPartition, FaultLink:
			if net < 2 {
				why = "fewer than 2 nodes"
			}
		}
		if why != "" {
			assert.Reachable("sim: cannot inject: nemesis: "+k+": "+why, nil)
		} else {
			assert.Reachable("sim: can inject: nemesis: "+k, nil)
		}
	}
}

// pickSome returns each element of xs with probability 1/2, and at least
// one if xs is not empty.
func pickSome[T any](xs []T) []T {
	var out []T
	for _, x := range xs {
		if runtime_rand()&1 == 0 {
			out = append(out, x)
		}
	}
	if len(out) == 0 && len(xs) > 0 {
		out = append(out, xs[runtime_rand()%uint64(len(xs))])
	}
	return out
}

// pickKind picks an enabled fault kind. A process kill is three times as
// likely as any other kind, and so as a power failure: it is the common
// crash, while power failures find the fsync bugs.
func (nm *Nemesis) pickKind() string {
	weight := func(k string) int {
		if k == FaultKillProcess {
			return 3
		}
		return 1
	}
	total := 0
	for _, k := range nm.kinds {
		total += weight(k)
	}
	r := Intn(total)
	for _, k := range nm.kinds {
		if r -= weight(k); r < 0 {
			return k
		}
	}
	return nm.kinds[0]
}

// effect draws a network effect from the enabled modes.
func (nm *Nemesis) effect() Effect {
	m := nm.modes[Intn(len(nm.modes))]
	e := Effect{Mode: m}
	if m == Slowed {
		e.Mean = time.Duration(50+Intn(950)) * time.Millisecond
		e.Dev = e.Mean / 2
	}
	return e
}

func (nm *Nemesis) inject() {
	if len(nm.kinds) == 0 {
		return
	}
	w := nm.w
	net, nodes := nm.netTargets(), nm.nodeTargets()
	// aim returns the index of the node in ns that a fault hits.
	aim := func(ns []*Node) int {
		if i := slices.Index(ns, nm.focus); i >= 0 {
			return i
		}
		return Intn(len(ns))
	}
	maxDown := max(1, (len(nodes)-1)/2) // a minority
	if len(net) == 0 {
		return
	}
	kind := nm.pickKind()
	dur := expRand(nm.dur)
	ev := Event{Kind: kind, Start: true}
	var undo func()
	switch kind {
	case FaultPartition:
		if len(net) < 2 {
			return
		}
		// Split into 2 groups, or 3 with 3 nodes or more.
		k := 2
		if len(net) >= 3 && Intn(3) == 0 {
			k = 3
		}
		groups := make([][]*Node, k)
		perm := make([]int, len(net))
		for i := range perm {
			perm[i] = i
		}
		for i := len(perm) - 1; i > 0; i-- {
			j := Intn(i + 1)
			perm[i], perm[j] = perm[j], perm[i]
		}
		for i, p := range perm {
			g := i
			if i >= k {
				g = Intn(k)
			}
			groups[g] = append(groups[g], net[p])
		}
		f := w.PartitionGroups(nm.effect(), Intn(3) == 0, groups...)
		ev.Net, ev.Desc = f, f.String()
		undo = f.Heal
		assert.Reachable("nemesis: partition "+f.eff.Mode.String(), nil)
	case FaultClog:
		n := net[aim(net)]
		in, out := true, true
		switch Intn(3) {
		case 1:
			in = false
		case 2:
			out = false
		}
		f := w.Clog(n, in, out, nm.effect())
		ev.Net, ev.Node, ev.Desc = f, n, f.String()
		undo = f.Heal
		assert.Reachable("nemesis: clog "+f.eff.Mode.String(), nil)
	case FaultLink:
		if len(net) < 2 {
			return
		}
		i := aim(net)
		j := (i + 1 + Intn(len(net)-1)) % len(net)
		a, b := net[i], net[j]
		eff := nm.effect()
		var f *NetFault
		if Intn(3) == 0 {
			f = w.Cut(a, b, eff)
		} else {
			f = w.addFault(eff, [][2]int{{a.index, b.index}, {b.index, a.index}}, "link "+eff.Mode.String()+" "+a.name+"<->"+b.name)
		}
		ev.Net, ev.Desc = f, f.String()
		undo = f.Heal
		assert.Reachable("nemesis: link "+eff.Mode.String(), nil)
	case FaultRestore:
		if len(w.Faults()) == 0 {
			return
		}
		nm.endNet()
		ev.Desc = "restore"
		assert.Reachable("nemesis: restore", nil)
		nm.event(ev)
		return
	case FaultKill, FaultKillProcess, FaultStop, FaultPause, FaultThrottle:
		if len(nm.down) >= maxDown {
			return
		}
		focused := slices.Contains(nodes, nm.focus)
		var up []*Node
		for _, n := range nodes {
			if n.Up() && !n.Paused() && !n.Throttled() && !nm.down[n] && (!focused || n == nm.focus) {
				up = append(up, n)
			}
		}
		if len(up) == 0 {
			return
		}
		n := up[Intn(len(up))]
		ev.Node, ev.Desc = n, kind+" "+n.name
		nm.down[n] = true
		switch kind {
		case FaultKill:
			n.Kill()
		case FaultKillProcess:
			n.KillProcess()
		case FaultStop:
			n.Stop(time.Duration(Intn(2000)) * time.Millisecond)
		case FaultPause:
			n.Pause()
		case FaultThrottle:
			n.Throttle(2 + Intn(9))
		}
		undo = func() {
			delete(nm.down, n)
			switch kind {
			case FaultPause:
				n.Resume()
			case FaultThrottle:
				n.Throttle(1)
			default:
				if !n.Up() && !n.removed {
					nm.calls++
					nm.restarting = int32(n.index + 1)
					nm.cfg.Restart(n)
					nm.restarting = 0
					nm.calls++
				}
			}
		}
		assert.Reachable("nemesis: "+kind, nil)
	case FaultDisk:
		if len(nodes) == 0 {
			return
		}
		n := nodes[aim(nodes)]
		d := n.disk
		if d.slow > 1 || d.stallUntil > now() {
			return
		}
		ev.Node = n
		if Intn(3) == 0 {
			runtime_call(func() { d.stallUntil = now() + int64(dur) })
			ev.Desc = "disk stalled " + n.name
			assert.Reachable("nemesis: disk stalled", nil)
		} else {
			slow := int64(logUniform(10, 1000))
			runtime_call(func() { d.slow = slow })
			ev.Desc = fmt.Sprintf("disk slowed %s x%d", n.name, slow)
			assert.Reachable("nemesis: disk slowed", nil)
		}
		undo = func() {
			runtime_call(func() { d.slow, d.stallUntil = 0, 0 })
			n.wakeDisk()
		}
	case FaultClockSkip:
		d := time.Duration(1+Intn(3600)) * time.Second
		forward := slices.Contains(nm.cfg.Disable, FaultClockSkipBack)
		if Intn(2) == 0 && !forward {
			d = -d
		}
		w.SkipClock(d)
		ev.Skip, ev.Desc = d, "clockskip "+d.String()
		if d < 0 {
			assert.Reachable("nemesis: clock skips back", nil)
		} else {
			assert.Reachable("nemesis: clock skips forward", nil)
		}
		if Intn(2) == 0 || forward {
			nm.event(ev)
			return
		}
		undo = func() { w.SkipClock(-d) }
	}
	if len(nm.active) > 0 {
		assert.Reachable("nemesis: overlapping faults", nil)
	}
	nm.event(ev)
	a := &activeFault{end: time.Now().Add(dur), ev: ev, undo: undo}
	i := slices.IndexFunc(nm.active, func(x *activeFault) bool { return x.end.After(a.end) })
	if i < 0 {
		i = len(nm.active)
	}
	nm.active = slices.Insert(nm.active, i, a)
}

func (nm *Nemesis) end(a *activeFault) {
	a.undo()
	ev := a.ev
	ev.Start = false
	nm.event(ev)
}

// endNet ends the nemesis's network faults.
func (nm *Nemesis) endNet() {
	kept := nm.active[:0]
	var ended []*activeFault
	for _, a := range nm.active {
		if a.ev.Net != nil {
			ended = append(ended, a)
		} else {
			kept = append(kept, a)
		}
	}
	nm.active = kept
	for _, a := range ended {
		nm.end(a)
	}
}

// endAll ends every fault the nemesis injected except clock skips, which
// it keeps as permanent.
func (nm *Nemesis) endAll() {
	active := nm.active
	nm.active = nil
	for _, a := range active {
		if a.ev.Kind == FaultClockSkip {
			continue
		}
		nm.end(a)
	}
}

// event calls the Event callback. It stays inlined, so that counting the
// call (see nemesisNode) adds no tick.
func (nm *Nemesis) event(ev Event) {
	if ev.Start {
		nm.w.logEvent("nemesis", "start", ev.Desc)
	} else {
		nm.w.logEvent("nemesis", "end", ev.Desc)
	}
	if nm.cfg.Event != nil {
		nm.calls++
		nm.cfg.Event(ev)
		nm.calls++
	}
}

// nemesisNode answers the runtime, which watches the harness callbacks of
// the nemesis whose calls field is at calls (runtime_watch), so that it
// can fail the program when one waits for a node the nemesis paused,
// which only the nemesis's goroutine would resume. calls is odd while a
// callback runs. For the node with the given handle, nemesisNode returns
// its name and whether the nemesis paused it; for handle 0, it names the
// callback running.
//
// The nemesis tells the runtime about its callbacks only through calls
// and restarting, which sit in padding of the struct, and with no
// function call, so that the watch changes no allocation size and no
// tick, and so no schedule.
func nemesisNode(calls unsafe.Pointer, handle int32) (name string, paused bool) {
	nm := (*Nemesis)(unsafe.Add(calls, -int(unsafe.Offsetof((*Nemesis)(nil).calls))))
	if handle == 0 {
		if nm.restarting != 0 {
			return "Restart callback for node " + nm.w.nodes[nm.restarting-1].name, false
		}
		return "Event callback", false
	}
	inc := nm.w.incs[handle]
	if inc == nil {
		return "", false
	}
	n := inc.node
	return n.name, n.inc == inc && n.paused && nm.down[n]
}
