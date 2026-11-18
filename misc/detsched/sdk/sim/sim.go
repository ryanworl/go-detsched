// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

// Package sim runs several nodes of a distributed system in one process
// under GOEXPERIMENT=detsched, each with its own simulated network stack
// and disk, so that unmodified code using package net (TCP and UDP) and
// package os (files and directories) talks over a simulated network and
// stores data on a simulated disk.
//
// A node is a set of goroutines. Node.Start runs a function in a new
// goroutine that belongs to the node; goroutines it starts belong to the
// node too, and so do goroutines started by its time.AfterFunc timers. On a
// node's goroutines, system calls on paths and sockets go to the node's
// simulated kernel. Descriptors the simulation returns work from any
// goroutine. Other goroutines, such as the test's, use the real OS, except
// that while a World is active they cannot open an Internet socket: a dial
// or listen there would wait on the real network, which the deterministic
// scheduler does not wait for. The call fails with EPERM, and the first one
// prints why on standard error; run such code with Node.Go, or on a node.
//
// The network carries TCP and UDP between the nodes' IPv4 addresses. Each
// node also has its own loopback. Messages take a latency drawn from the
// fault stream, in the order sent on one connection, and in any order
// across connections.
// Partitions hold data on existing connections until they heal, or for
// 30 seconds, when the connection fails; connecting across a partition or
// to a node that is down times out after 20 seconds.
//
// Each node has a disk that survives restarts. Writes stay in a page cache
// until fsync. Killing a node (Node.Kill) is a power failure: it loses
// unsynced data, so each file reverts to its contents at its last fsync,
// and the namespace (creates, renames, removes) to its state at the last
// fsync of any file or directory on the disk, as with an ordered journal.
//
// A node's life cycle: World.AddNode adds it, down; Node.Start powers it on
// and runs its code; Node.Kill crashes the machine; Node.KillProcess crashes
// only its process, so the kernel keeps unsynced writes and closes its
// connections; Node.Stop shuts it down gracefully; Node.Pause and
// Node.Resume stop and continue its goroutines while it stays on the
// network, as SIGSTOP and SIGCONT would; Node.Start again restarts it with
// what its disk kept; World.RemoveNode takes it off the network, and
// World.ReAddNode returns it with its disk, unless Node.WipeDisk emptied it.
//
// A crash freezes the node's goroutines where they are: they never run
// again, and their memory stays as it was. Nodes share the memory of the
// process, such as the global state of the libraries they use, and so the
// mutexes in it: a frozen goroutine that holds a sync.Mutex or RWMutex
// that a goroutine of a live node waits for runs on, as a zombie whose
// system calls fail with EIO, until it releases it, and then freezes
// again. So a lock that harness code shares between nodes must not be
// held across a call into a node's code: a zombie that waits there for
// its node's frozen goroutines never releases it. Only those two types run
// on as zombies: a lock made of a channel, such as a chan struct{} of
// capacity 1, that a frozen goroutine holds stays held, and every live
// node that takes it waits forever. Code written for upstream synctest
// uses such locks because upstream never counts a goroutine waiting for a
// sync.Mutex as durably blocked; under the experiment it does, so use a
// sync.Mutex.
//
// All of the simulation's random choices come from the runtime's fault
// stream, which GODETSEED seeds. Each World draws its network's timing,
// and each node its processor (Node.SetCPU). The simulation's own faults
// are buggify sites (see package detsched/sdk/buggify), so each run
// enables a different subset of them.
//
// A run that fails prints the fault timeline after its output: when each
// node started, crashed, stopped, paused, and lost data, each disk fault,
// each clock skip, and each fault of a Nemesis, with the simulated time.
// A run that passes prints none of it.
//
// Create the World inside a synctest bubble, so that time is simulated,
// and close it before the bubble ends. The package exists on Linux on
// amd64 and arm64 and on macOS on arm64. Without the experiment, NewWorld
// panics; use Supported to skip.
package sim

import (
	"bytes"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/internal/hook"
)

// Config configures a World.
type Config struct {
	// OnPanic, if set, makes an unrecovered panic on a node's goroutine
	// crash only that node, as KillProcess does, instead of the whole
	// program, as a panic crashes only the node's process on a real
	// cluster; so do fatal errors the runtime raises for user code, such
	// as unlocking an unlocked mutex, and os.Exit, as log.Fatal calls it,
	// with the message "os.Exit(<code>)". The panic is printed to
	// standard error and reported as assert.Reachable("node panicked:
	// <first line of the message>"), and then OnPanic is called with the
	// node, the message, and the goroutine's stack, so that a harness can
	// tell a crash its system meant, which it ignores, from a bug, such as
	// a nil pointer dereference, which it fails. The goroutine's deferred
	// calls have run by then. Panics on goroutines of no node still end
	// the program, and so does every panic without OnPanic.
	OnPanic func(n *Node, msg string, stack []byte)
}

// The network's defaults: links have a one-way latency of latency plus an
// exponentially distributed delay with mean jitter (see World.SetLink to
// change a link), data that cannot get through for tcpTimeout fails its
// connection with ETIMEDOUT, and a connection attempt that gets no answer
// fails after connectTimeout, as on Linux.
const (
	latency        = time.Millisecond
	jitter         = time.Millisecond
	tcpTimeout     = 30 * time.Second
	connectTimeout = 20 * time.Second
)

// World is a simulated network of nodes.
type World struct {
	cfg    Config
	nodes  []*Node
	byIP   map[[4]byte]*Node
	incs   map[int32]*incarnation // by node handle
	failed map[string]bool        // assertions that failed, which the timeline shows
	quiet  int64                  // no buggify faults before then (calm)

	fds map[int]*fdesc

	events   map[uint64]func()
	nextConn uint64
	allConns map[*conn]bool
	maps     []mapping

	links     map[[2]int]Link // baselines of directed links, by node index
	faults    []*NetFault     // active network faults, oldest first
	nextFault int
	waiting   []func()    // connection attempts waiting for a link to change
	udpHeld   []heldDgram // datagrams a Jammed fault holds
	grain     int64       // message arrivals are rounded up to a multiple of it
	start     int64       // when the World began, for logEvent
	skew      time.Duration
	closing   bool // Close is killing the nodes
	noNodeMsg bool // printed noNodeSocket's message
}

// Node is a machine in a World: an address, a disk, and, while it is up,
// goroutines.
type Node struct {
	w              *World
	index          int
	name           string
	ip             [4]byte
	disk           *disk
	inc            *incarnation // nil while down
	kernel         bool         // its kernel runs: from Start to a power failure
	paused         bool
	removed        bool
	throttle       int // generation of Throttle calls
	throttled      bool
	throttlePaused bool          // Throttle's goroutine paused the node
	throttleStop   chan struct{} // closed to end Throttle's goroutine
	onStop         []func()
	cores          int               // its processor's cores; UnlimitedCores
	speed          float64           // its processor's speed, times the reference core
	nat            map[uint16]uint16 // with SetNAT, UDP public port by local port
	natRev         map[uint16]uint16 // local port by public port
	natNext        uint16            // the next public port to try
	starts         int               // times Start started it
}

// An incarnation is a node from one start to its crash.
type incarnation struct {
	node   *Node
	handle int32
	alive  bool
	cwd    *inode
	ports  map[uint16]*socket // bound and listening TCP sockets
	uports map[uint16]*socket // bound UDP sockets
	socks  map[*socket]bool
	nextEp uint16
	// stopping is set once Stop ran the OnStop functions.
	stopping bool
}

// pid is the incarnation's process ID, which os.Getpid returns on its
// goroutines: each start of a node is a new process. The real process ID
// differs from run to run, and programs mix it into random seeds.
func (inc *incarnation) pid() int { return 1000 + int(inc.handle) }

var (
	world      *World
	nextHandle int32  // node handles are never reused, also across worlds
	nextEvent  uint64 // nor are event IDs
)

// Supported reports whether the toolchain supports simulation.
func Supported() bool { return supported }

// NewWorld creates a world with no nodes and routes system calls of its
// nodes to it. Only one world exists at a time. Call it inside a synctest
// bubble, and call Close before the bubble ends.
func NewWorld(cfg Config) *World {
	if !supported {
		panic("sim: requires GOEXPERIMENT=detsched")
	}
	if world != nil {
		panic("sim: a World already exists")
	}
	w := &World{
		cfg:      cfg,
		byIP:     make(map[[4]byte]*Node),
		incs:     make(map[int32]*incarnation),
		fds:      make(map[int]*fdesc),
		events:   make(map[uint64]func()),
		allConns: make(map[*conn]bool),
		links:    make(map[[2]int]Link),
		failed:   make(map[string]bool),
	}
	// Handling a message takes no simulated time, so without rounding
	// two events almost never fall at the same instant, where the
	// scheduler decides how their handlers interleave; on a real machine
	// handlers take time and overlap. Half of the worlds round message
	// arrivals up to whole milliseconds, so that messages, and timers set
	// to whole milliseconds, coincide often.
	if Intn(2) == 0 {
		w.grain = int64(time.Millisecond)
	}
	w.start = now()
	runtime_call(func() { world = w })
	register()
	runtime_clock(true, 0)
	runtime_onPanic(onPanic)
	runtime_busyOn(true)
	hook.AssertFailed = w.assertFailed
	w.logEvent("world", "new")
	return w
}

// assertFailed puts the first failure of each assertion in the fault
// timeline, under the node whose goroutine failed it.
func (w *World) assertFailed(msg string) {
	name, first := "world", false
	runtime_call(func() {
		if first = !w.failed[msg]; first {
			w.failed[msg] = true
			if inc := w.incs[runtime_node()]; inc != nil {
				name = inc.node.name
			}
		}
	})
	if first {
		w.logEvent(name, "assertion failed:", msg)
	}
}

// Close crashes every node that is up, freezing its goroutines, unmaps
// the world's files, and stops routing system calls to the world.
func (w *World) Close() {
	w.reportBusy()
	w.logEvent("world", "close")
	w.closing = true // no disk outlives the world: skip power failures
	for _, n := range w.nodes {
		if n.inc != nil {
			n.Kill()
		}
	}
	runtime_call(func() {
		w.unmap(func(mapping) bool { return true })
		world = nil
	})
	unregister()
	runtime_clock(false, 0)
	runtime_onPanic(nil)
	runtime_busyOn(false)
	hook.AssertFailed = nil
}

// onPanic crashes the node of a goroutine that panicked, if the world's
// Config has OnPanic. The runtime runs it with the node paused and the
// goroutine out of the node, so it may yield; the goroutine parks for
// good once onPanic returns, if the node crashed.
func onPanic(node int32, v any) {
	w := world
	if w == nil || w.cfg.OnPanic == nil {
		return
	}
	inc := w.incs[node]
	if inc == nil || inc.node.inc != inc {
		return
	}
	msg, ok := v.(string)
	if !ok {
		msg = fmt.Sprint(v)
	}
	first, _, _ := strings.Cut(msg, "\n")
	stack := debug.Stack()
	// Not through os.Stderr: a goroutine of a crashed node may have
	// frozen holding its lock.
	syscall.Write(2, fmt.Appendf(nil, "sim: node %s panicked: %s\n\n%s\n", inc.node.name, msg, stack))
	w.logEvent(inc.node.name, "panicked:", first)
	assert.Reachable("node panicked: "+first, nil)
	w.cfg.OnPanic(inc.node, msg, stack)
	inc.node.crash(true)
}

// SkipClock moves the wall clock of every node by d, forward or back, on
// top of earlier skips, as an NTP step would. Monotonic time, which
// time.Since, timers, and deadlines use, is not affected. To make a skip
// temporary, skip back by -d later.
func (w *World) SkipClock(d time.Duration) {
	w.skew += d
	w.logEvent("clock", "skip", d.String())
	runtime_clock(true, int64(w.skew))
}

// ClockSkew returns the sum of the clock skips so far.
func (w *World) ClockSkew() time.Duration { return w.skew }

// Elapsed returns the simulated time since the World began, which the
// fault timeline's lines start with and clock skips do not move. Stamp
// harness and SUT log lines with it to line them up with the timeline;
// the wall clock (time.Now) jumps with each skip.
func (w *World) Elapsed() time.Duration { return time.Duration(now() - w.start) }

// AddNode adds a node with the given name and IPv4 address, such as
// "10.0.0.1". The node starts down, with an empty disk; call Start.
func (w *World) AddNode(name, ip string) *Node {
	a, ok := parseIPv4(ip)
	if !ok || a[0] == 127 || a == [4]byte{} {
		panic("sim: bad node address " + ip)
	}
	if w.byIP[a] != nil {
		panic("sim: duplicate node address " + ip)
	}
	n := &Node{w: w, index: len(w.nodes), name: name, ip: a}
	n.cores, n.speed = drawCPU()
	runtime_call(func() {
		n.disk = newDisk(len(w.nodes) + 1)
		n.disk.drawDisk()
		w.nodes = append(w.nodes, n)
		w.byIP[a] = n
	})
	return n
}

// Name returns the node's name.
func (n *Node) Name() string { return n.name }

// IP returns the node's address.
func (n *Node) IP() string {
	return strconv.Itoa(int(n.ip[0])) + "." + strconv.Itoa(int(n.ip[1])) + "." + strconv.Itoa(int(n.ip[2])) + "." + strconv.Itoa(int(n.ip[3]))
}

// Up reports whether the node is running (or paused).
func (n *Node) Up() bool { return n.inc != nil }

// Paused reports whether the node is paused.
func (n *Node) Paused() bool { return n.paused }

// RemoveNode kills n if it is up and takes it off the network: its address
// no longer reaches anything, and the nemesis no longer faults it. Its disk
// stays, for ReAddNode.
func (w *World) RemoveNode(n *Node) {
	if n.inc != nil {
		n.Kill()
	}
	runtime_call(func() {
		n.removed = true
		delete(w.byIP, n.ip)
	})
}

// ReAddNode puts a removed node back on the network, down, at its address
// and with the disk it had when removed, as a machine taken out of service
// and later returned would be. Call Start.
func (w *World) ReAddNode(n *Node) {
	if !n.removed {
		panic("sim: node " + n.name + " was not removed")
	}
	if w.byIP[n.ip] != nil {
		panic("sim: duplicate node address " + n.IP())
	}
	runtime_call(func() {
		n.removed = false
		w.byIP[n.ip] = n
	})
}

// SetIP moves a node that is down to a new IPv4 address, as a machine
// that gets another address from DHCP when it boots would; its old
// address then reaches nothing.
func (n *Node) SetIP(ip string) {
	if n.inc != nil {
		panic("sim: node " + n.name + " is up")
	}
	a, ok := parseIPv4(ip)
	if !ok || a[0] == 127 || a == [4]byte{} {
		panic("sim: bad node address " + ip)
	}
	if a == n.ip {
		return
	}
	if n.w.byIP[a] != nil {
		panic("sim: duplicate node address " + ip)
	}
	runtime_call(func() {
		if !n.removed {
			delete(n.w.byIP, n.ip)
			n.w.byIP[a] = n
		}
		n.ip = a
	})
}

// WipeDisk replaces the disk of a node that is down with an empty one.
func (n *Node) WipeDisk() {
	if n.inc != nil {
		panic("sim: node " + n.name + " is up")
	}
	runtime_call(func() {
		d := newDisk(n.index + 1)
		d.latency, d.bandwidth = n.disk.latency, n.disk.bandwidth
		n.disk = d
	})
}

// Start powers the node on and runs f in a new goroutine that belongs to
// it. The node's disk holds what survived its last crash or stop.
func (n *Node) Start(f func()) {
	if n.inc != nil {
		panic("sim: node " + n.name + " is already up")
	}
	if n.removed {
		panic("sim: node " + n.name + " was removed")
	}
	if world != n.w {
		// Its goroutines would use the real OS.
		panic("sim: node " + n.name + " started after its World was closed")
	}
	n.onStop = nil
	var inc *incarnation
	runtime_call(func() {
		nextHandle++
		inc = &incarnation{
			node:   n,
			handle: nextHandle,
			alive:  true,
			cwd:    n.disk.root,
			ports:  make(map[uint16]*socket),
			uports: make(map[uint16]*socket),
			socks:  make(map[*socket]bool),
			nextEp: 32768,
		}
		n.inc, n.kernel = inc, true
		n.w.incs[inc.handle] = inc
		n.w.netChanged()
	})
	n.starts++
	runtime_nodeName(inc.handle, n.name+" (start "+strconv.Itoa(n.starts)+")")
	n.applyCPU()
	n.w.logEvent(n.name, "start")
	goAs(inc, f)
}

// Go runs f in a new goroutine that belongs to the node, if the node is
// up, and reports whether it did. Use it to act as the node from outside,
// such as a test that calls into the node's code: if the node crashes, the
// goroutine freezes with it.
func (n *Node) Go(f func()) bool {
	inc := n.inc
	if inc == nil {
		return false
	}
	goAs(inc, f)
	return true
}

// Goroutines returns how many goroutines the node's current incarnation
// has, or 0 while it is down, so that a harness can check that closing
// what runs on a node leaves no goroutine behind. Goroutines of earlier
// incarnations, frozen by a crash, do not count.
func (n *Node) Goroutines() int {
	inc := n.inc
	if inc == nil {
		return 0
	}
	return runtime_count(inc.handle)
}

// Stacks returns the stacks of the goroutines Goroutines counts, as
// runtime.Stack formats them, so that a harness can say what was left
// behind.
func (n *Node) Stacks() string {
	inc := n.inc
	if inc == nil {
		return ""
	}
	for buf := make([]byte, 64<<10); ; buf = make([]byte, 2*len(buf)) {
		if k := runtime_stacks(inc.handle, buf); k < len(buf) {
			return string(buf[:k])
		}
	}
}

// goAs runs f in a new goroutine of incarnation inc. The node may crash
// before it starts, as goroutines switch: the goroutine then freezes.
func goAs(inc *incarnation, f func()) {
	old := runtime_setNode(inc.handle)
	go f()
	runtime_setNode(old)
}

// Kill crashes the node as a power failure would: its goroutines freeze,
// its disk loses everything not synced, and its connections go silent, so
// peers notice only through timeouts, or through resets once the node is up
// again. On a node whose process is down but whose kernel still runs, as
// after KillProcess or a panic, it cuts the power: the disk loses what it
// had not synced.
func (n *Node) Kill() {
	if n.inc == nil {
		runtime_call(func() {
			if n.kernel {
				n.kernel = false
				n.w.logEvent(n.name, "power failure (process down)")
				n.disk.powerFail()
				n.w.syncMaps(n.disk)
			}
		})
		return
	}
	n.crash(false)
}

// KillProcess crashes the node's process, as SIGKILL would: its goroutines
// freeze, and the kernel, which keeps running, closes its sockets, so peers
// see their connections end, and keeps its unsynced writes.
func (n *Node) KillProcess() { n.crash(true) }

// OnStop registers f to run, in a goroutine of the node, when Stop shuts the
// node down, as a SIGTERM handler would. Registrations last until the node
// next starts.
func (n *Node) OnStop(f func()) {
	runtime_call(func() { n.onStop = append(n.onStop, f) })
}

// Stop shuts the node down gracefully: it runs the OnStop functions, waits
// up to grace for the node's goroutines to exit, and then ends the process
// as KillProcess does, freezing goroutines still left. Call it from outside
// the node. A Stop while another one shuts the same process down runs no
// OnStop function again, as a second SIGTERM to a process that handles
// the first runs no second shutdown, and ends the process when either
// grace runs out.
func (n *Node) Stop(grace time.Duration) {
	inc := n.inc
	if inc == nil {
		return
	}
	n.w.logEvent(n.name, "stop")
	n.Throttle(1)
	if n.paused {
		n.Resume()
	}
	if !inc.stopping {
		inc.stopping = true
		for _, f := range n.onStop {
			goAs(inc, f)
		}
	}
	for end := time.Now().Add(grace); runtime_count(inc.handle) > 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if n.inc == inc {
		n.crash(true)
	}
}

// Pause stops the node's goroutines from running while the node stays on
// the network: its kernel still accepts connections and buffers data, so
// peers see their requests time out.
func (n *Node) Pause() {
	if n.inc == nil || n.paused {
		return
	}
	n.paused = true
	n.w.logEvent(n.name, "pause")
	runtime_pause(n.inc.handle)
}

// Resume lets the goroutines of a paused node run again.
func (n *Node) Resume() {
	if n.inc == nil || !n.paused {
		return
	}
	n.paused = false
	n.w.logEvent(n.name, "resume")
	runtime_resume(n.inc.handle)
}

// SetDiskFull makes the node's disk full, or no longer full: while it is,
// writes that extend a file, and creating files and directories, fail with
// ENOSPC.
func (n *Node) SetDiskFull(full bool) {
	if full != n.disk.full {
		n.w.logEvent(n.name, "disk full:", strconv.FormatBool(full))
	}
	runtime_call(func() { n.disk.full = full })
}

// wakeDisk ends now the waits of the node's goroutines for its disk.
func (n *Node) wakeDisk() {
	if n.inc != nil {
		runtime_wake(n.inc.handle)
	}
}

// SetSyncFaultExempt exempts the files whose name ends with one of
// suffixes from the fsync failures "sim.disk.fsyncfail" injects on the
// node's disk, as a harness may to keep a known bug in a program's
// handling of one kind of file out of its runs; with no suffixes, every
// file may fail again. The fault's random draws stay the same, and so
// does the rest of the schedule.
func (n *Node) SetSyncFaultExempt(suffixes ...string) {
	runtime_call(func() { n.disk.syncExempt = suffixes })
}

// SyncFailed reports whether an fsync on the node's disk failed since the
// node last lost power. The page cache then holds writes the disk never
// got, which a process restarted without a power cycle reads back as if
// they were durable, as on Linux.
func (n *Node) SyncFailed() bool {
	var failed bool
	runtime_call(func() { failed = n.disk.syncFailed })
	return failed
}

// Throttle slows the node down by factor, as an overloaded machine would:
// its goroutines run for one period of 10ms in every factor periods, and
// are paused for the rest, so it reacts late. Throttle(1) ends it, as
// does a crash.
func (n *Node) Throttle(factor int) {
	n.throttle++
	n.endThrottle()
	n.throttled = factor > 1
	if n.throttlePaused {
		n.throttlePaused = false
		if !n.paused && n.inc != nil {
			runtime_resume(n.inc.handle)
		}
	}
	if factor <= 1 || n.inc == nil {
		return
	}
	gen, inc := n.throttle, n.inc
	stop := make(chan struct{})
	n.throttleStop = stop
	// sleep sleeps for d, and reports whether the throttle still holds:
	// Throttle or a crash ends it, and wakes the goroutine, so that it
	// does not outlive the bubble.
	sleep := func(d time.Duration) bool {
		select {
		case <-stop:
			return false
		case <-time.After(d):
		}
		return n.throttle == gen && n.inc == inc
	}
	old := runtime_setNode(0)
	go func() {
		const period = 10 * time.Millisecond
		for {
			if n.throttle != gen || n.inc != inc {
				return // Throttle or a crash ended it before it began
			}
			if !n.paused {
				runtime_pause(inc.handle)
				n.throttlePaused = true
			}
			if !sleep(time.Duration(factor-1) * period) {
				return // Throttle resumed the node, or it crashed
			}
			if n.throttlePaused {
				n.throttlePaused = false
				if !n.paused {
					runtime_resume(inc.handle)
				}
			}
			if !sleep(period) {
				return
			}
		}
	}()
	runtime_setNode(old)
}

// endThrottle wakes the goroutine of the last Throttle, which then exits.
func (n *Node) endThrottle() {
	if n.throttleStop != nil {
		close(n.throttleStop)
		n.throttleStop = nil
	}
}

// Throttled reports whether the node is throttled.
func (n *Node) Throttled() bool { return n.throttled && n.inc != nil }

func (n *Node) crash(process bool) {
	// End the throttle first: the goroutine running crash may yield on
	// the way, and must not leave the node half crashed meanwhile, with
	// its memory unmapped and its descriptors closed but its goroutines
	// still running. Then crash the node and freeze its goroutines in one
	// step, which nothing interrupts.
	n.endThrottle()
	inc := n.inc
	if inc == nil {
		return
	}
	self := runtime_node() == inc.handle
	if process {
		n.w.logEvent(n.name, "process crash")
	} else {
		n.w.logEvent(n.name, "power failure")
	}
	runtime_call(func() {
		n.w.crash(inc, process)
		n.inc = nil
		n.paused = false
		n.throttled = false
		n.throttlePaused = false
		runtime_crash(inc.handle)
	})
	if self {
		runtime_halt()
	}
}

// Rand returns a number from the fault stream, for test code that injects
// faults, such as a nemesis choosing which node to crash. Each branch point
// of the tape reseeds the stream.
func Rand() uint64 { return runtime_rand() }

// Branch forks a branch of the timeline that ends it, as Antithesis runs
// an eventually or finally check. It returns true in the branch, which
// should run its check and exit, and false in the timeline, which waits
// for the branch to exit and then goes on as if it had not forked. A
// branch that exits with a nonzero status fails the assertion msg in the
// timeline; its output is written only then. Branch returns false without
// forking where the process cannot fork, such as in stock builds, with
// cgo, or in a branch, and once an assertion failed, as the branch would
// fail too.
//
// A debugger does not follow the fork. With GODETBRANCH=N in the
// environment, the runtime forks no branch: the Nth branch it would fork
// runs in the process itself, with the same schedule as the forked child,
// and every other call returns false, as if its branch had passed.
// Branches are numbered from 1, in the order the timeline forks them;
// BranchNumber returns the number in a branch.
func Branch(msg string) bool {
	n := runtime_branch(msg)
	if n > 0 {
		branchNumber = n
	}
	return n > 0
}

// BranchNumber returns the number of the branch of the timeline that the
// process runs, which GODETBRANCH takes to run it again in place, or 0
// if the process runs no branch.
func BranchNumber() int { return branchNumber }

var branchNumber int

// Intn returns a number in [0, n) from the fault stream.
func Intn(n int) int { return int(runtime_rand() % uint64(n)) }

func parseIPv4(s string) ([4]byte, bool) {
	var a [4]byte
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return a, false
	}
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 {
			return a, false
		}
		a[i] = byte(v)
	}
	return a, true
}

// event schedules f to run after d on the bubble's clock, on the system
// stack.
func (w *World) event(d int64, f func()) {
	d = w.grid(now()+d) - now()
	nextEvent++
	w.events[nextEvent] = f
	runtime_after(d, nextEvent)
}

func timerFired(id uint64) {
	w := world
	if w == nil {
		return
	}
	f := w.events[id]
	if f == nil {
		return
	}
	delete(w.events, id)
	f()
}

func now() int64 { return runtime_now() }

// logEvent records a fault or a node's change of state in the fault
// timeline, with the simulated time since the World began, such as
// "sim: 12.004000000 n1: power failure". The runtime writes the timeline
// to standard error when the program fails (see runtime_failLog). Only
// strings longer than 255 bytes are cut. logEvent records on the system
// stack, which counts no ticks, into a buffer that holds no pointers, so
// that the timeline changes neither the schedule nor the heap.
func (w *World) logEvent(name string, what ...string) {
	if w == nil || w.closing {
		return // the World's nodes go down with it, and a disk outlives it
	}
	runtime_call(func() {
		var b [256]byte
		line := append(b[:0], "sim: "...)
		t := now() - w.start
		line = strconv.AppendInt(line, t/1e9, 10)
		line = append(line, '.')
		var fb [20]byte
		line = append(line, strconv.AppendInt(fb[:0], t%1e9+1e9, 10)[1:]...)
		line = append(line, ' ')
		line = append(line, name[:min(len(name), 64)]...)
		line = append(line, ':')
		for _, s := range what {
			line = append(line, ' ')
			line = append(line, s[:min(len(s), cap(line)-len(line)-1)]...)
		}
		line = append(line, '\n')
		if len(timeline)+len(line) > len(timelineBuf) {
			// Drop the older half.
			const dropped = "sim: (earlier events dropped)\n"
			old := timeline[len(timeline)/2:]
			if i := bytes.IndexByte(old, '\n'); i >= 0 {
				old = old[i+1:]
			}
			n := copy(timelineBuf[len(dropped):], old)
			copy(timelineBuf[:], dropped)
			timeline = timelineBuf[:len(dropped)+n]
		}
		timeline = append(timeline, line...)
	})
}

// timeline holds the lines of the fault timeline, in timelineBuf.
var (
	timelineBuf [128 << 10]byte
	timeline    = timelineBuf[:0]
)

// wallNow is the nodes' wall clock, with the world's clock skips, for file
// times.
func wallNow() int64 {
	t := now()
	if world != nil {
		t += int64(world.skew)
	}
	return t
}

// calm reports whether a nemesis has stopped faults for now
// (Nemesis.StopFaults or Stop), during which the simulation's own buggify
// faults, such as retransmission delays and disk errors, stay off too.
func calm() bool { return world != nil && now() < world.quiet }

// SetCalm keeps the simulation's buggify faults, such as disk errors and
// retransmission delays, off while on is true, as a nemesis's calm does,
// and turns them back on with false. package compose keeps them off
// while a test sets up, as Antithesis injects no fault before setup
// completes.
func (w *World) SetCalm(on bool) {
	runtime_call(func() {
		if on {
			w.quiet = 1<<63 - 1
		} else {
			w.quiet = 0
		}
	})
}

// grid rounds t up to a multiple of w.grain.
func (w *World) grid(t int64) int64 {
	if g := w.grain; g > 0 {
		t = (t + g - 1) / g * g
	}
	return t
}
