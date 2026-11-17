// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package compose runs a workload against a simulated cluster
// (detsched/sdk/sim) in the shape of Antithesis's Test Composer: a test
// registers commands of a few kinds, and the composer decides, from the
// fault stream, when each runs, what runs alongside it, and when faults
// are on, so that every run, and every branch of a guided search, tests
// a different timeline.
//
// A timeline has three phases:
//
//   - Setup: one First command, picked at random if there are several,
//     runs alone with no faults. Then the composer reports
//     lifecycle.SetupComplete, so that a search branches after setup.
//   - Drive: a sim.Nemesis runs storms of faults separated by calms while
//     the composer starts drivers. Parallel commands overlap, with each
//     other and with copies of themselves, up to a limit drawn for the
//     timeline. A Serial command runs alone: the composer stops starting
//     parallel ones and waits for those running to return first.
//     Anytime commands run alongside any driver. In a timeline with
//     Singleton commands, half the time one of them runs as the only
//     driver, and the drive phase ends when it returns. The drive phase
//     lasts from 15 seconds (or half of Config.Budget, if less) to all of
//     Config.Budget, drawn log-uniformly, and starts over
//     after each branch of a guided search (lifecycle.Branches).
//   - Terminal: one Eventually or Finally command, picked at random. As
//     in Antithesis, it runs on a branch of the timeline: at random times
//     during the drive phase, the composer forks the process with
//     sim.Branch, the branch runs the terminal phase and exits, and the
//     timeline goes on, so that one timeline checks recovery from many of
//     its states rather than only from where it ends. The mean time
//     between branches is drawn for each timeline from 2 to 30 seconds.
//     The timeline also runs the terminal phase itself when its drive
//     phase ends. A branch that fails fails the run, and the failure it
//     reports comes first in the output, with the GODETBRANCH setting
//     that replays the branch in one process, as a debugger needs. For an
//     Eventually command, the composer cancels the drivers' context,
//     waits a second, and then kills the client node they run on, freezing
//     whatever is still running: a goroutine of the client node may be
//     frozen before it ever saw the context done, so a check reads
//     Ctx.Context().Err() rather than wait for such a goroutine. For a
//     Finally command, it lets the drivers return on their own, up to a
//     minute, and if they do not, runs an Eventually command instead, as
//     Antithesis runs a
//     finally command only after every driver has completed. Then it stops
//     the faults, restarting the nodes the nemesis took down, unless the
//     command was registered with FaultsOn, in which case the storms go on
//     while it runs. It may be destructive: its branch, or the timeline,
//     ends with it.
//
// Commands run on a client node the composer adds to the world, named
// "client", so their system calls go to the simulation, and dialing a
// cluster node crosses the simulated network. The nemesis never kills,
// pauses, or throttles the client, but its network faults reach it: a
// partition can leave it with some cluster nodes and not others. A command that calls into a cluster node's code directly,
// such as through sim.Node.Go, runs that code on the cluster node, where
// killing the client does not stop it; such commands must return once
// Ctx.Context is done.
//
// Drivers should do a bounded amount of work and return, such as a few
// writes or one membership change, rather than loop until told to stop:
// the composer starts them again and again, so it decides the mix, and a
// Finally command can rely on every driver that started having finished.
//
// A command fails the run by returning an error, which the composer
// reports with assert.Always and t.Errorf, or by panicking, which the
// composer passes on, so that the run ends with the panic.
package compose

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/guide"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// Config configures a Composer. Zero fields take defaults.
type Config struct {
	// Faults configures the nemesis that runs during the drive phase.
	// The composer adds its client node to Faults.Exclude, which keeps
	// node faults off it but not network faults, and calls Faults.Event
	// too.
	Faults sim.NemesisConfig
	// Budget is the longest the drive phase may last, in simulated time
	// (default 2 minutes, at least 20 seconds).
	Budget time.Duration
}

const (
	drainTimeout = time.Minute      // how long a Finally command waits for the drivers
	clientIP     = "10.255.255.254" // the client node's address
)

// Option modifies a command.
type Option struct {
	faultsOn bool
	weight   int
}

// FaultsOn keeps storms going while an Eventually or Finally command runs,
// for checks that must hold while the system is degraded, such as that
// acknowledged data is never lost.
var FaultsOn = Option{faultsOn: true}

// Weight makes a Parallel, Serial, or Anytime command n times as likely to
// be picked as one of the same kind with the default weight of 1, such as
// a driver of rare administrative operations next to one of writes.
func Weight(n int) Option { return Option{weight: max(n, 1)} }

type kind int

const (
	first kind = iota
	parallel
	serial
	singleton
	anytime
	eventually
	finally
)

var kindNames = [...]string{"first", "parallel", "serial", "singleton", "anytime", "eventually", "finally"}

type command struct {
	kind     kind
	name     string
	f        func(*Ctx) error
	faultsOn bool
	weight   int
	index    int
}

// A run is one execution of a command.
type run struct {
	cmd   *command
	err   error
	panic any    // what the command panicked with, if it did
	stack []byte // where
}

// Composer runs a workload of commands with faults. Create it with New,
// register commands, and call Run.
type Composer struct {
	w    *sim.World
	cfg  Config
	cmds []*command

	t       *testing.T
	client  *sim.Node
	nm      *sim.Nemesis
	stopped bool // the nemesis was stopped
	fin     chan *run
	ctx     context.Context
	cancel  context.CancelFunc

	running  []*run         // drivers and anytime commands running
	active   [16]int        // nemesis faults active, by kind
	storms   int            // storms so far
	counts   map[string]int // runs per command, for the summary
	peak     int            // the most parallel drivers that ran at once
	branches int            // branches forked for the terminal phase
	branch   bool           // this process is such a branch
	failures []string
}

// New creates a composer for the cluster in w.
func New(w *sim.World, cfg Config) *Composer {
	if cfg.Budget <= 0 {
		cfg.Budget = 2 * time.Minute
	}
	return &Composer{w: w, cfg: cfg, counts: map[string]int{}}
}

func (c *Composer) add(k kind, name string, f func(*Ctx) error, opts []Option) {
	cmd := &command{kind: k, name: name, f: f, weight: 1, index: len(c.cmds)}
	for _, o := range opts {
		cmd.faultsOn = cmd.faultsOn || o.faultsOn
		if o.weight > 0 {
			cmd.weight = o.weight
		}
	}
	c.cmds = append(c.cmds, cmd)
}

// First registers a setup command, which runs alone, with no faults,
// before any other. Of several, one is picked for each run.
func (c *Composer) First(name string, f func(*Ctx) error) { c.add(first, name, f, nil) }

// Parallel registers a driver that may run alongside other parallel
// drivers, including copies of itself.
func (c *Composer) Parallel(name string, f func(*Ctx) error, opts ...Option) {
	c.add(parallel, name, f, opts)
}

// Serial registers a driver that runs with no other driver alongside.
func (c *Composer) Serial(name string, f func(*Ctx) error, opts ...Option) {
	c.add(serial, name, f, opts)
}

// Singleton registers a driver that runs once, as the only driver of its
// timeline.
func (c *Composer) Singleton(name string, f func(*Ctx) error) { c.add(singleton, name, f, nil) }

// Anytime registers a check that runs now and then during the drive
// phase, alongside any driver, such as that a read reflects the last
// acknowledged write.
func (c *Composer) Anytime(name string, f func(*Ctx) error, opts ...Option) {
	c.add(anytime, name, f, opts)
}

// Eventually registers a terminal check that runs after the drivers are
// killed, such as that the cluster recovers and converges.
func (c *Composer) Eventually(name string, f func(*Ctx) error, opts ...Option) {
	c.add(eventually, name, f, opts)
}

// Finally registers a terminal check that runs after every driver has
// returned on its own, such as that every acknowledged write is present.
func (c *Composer) Finally(name string, f func(*Ctx) error, opts ...Option) {
	c.add(finally, name, f, opts)
}

// Ctx is what a command gets from the composer.
type Ctx struct {
	c   *Composer
	cmd *command
	ctx context.Context
}

// Context is done when the composer ends the drive phase for an
// Eventually command. It is never done for setup and terminal commands.
func (x *Ctx) Context() context.Context { return x.ctx }

// Node returns the client node the command runs on.
func (x *Ctx) Node() *sim.Node { return x.c.client }

// Quiesce starts a calm that lasts at least d, or extends the current one,
// as ANTITHESIS_STOP_FAULTS does: every fault ends and nodes taken down
// restart. Use it to check mid-run that the system recovers. It does
// nothing outside the drive phase.
func (x *Ctx) Quiesce(d time.Duration) {
	if x.c.nm != nil && !x.c.stopped {
		x.c.nm.StopFaults(d)
	}
}

// CalmSince reports whether the drive phase's nemesis is in a calm, with
// every fault ended, and since when (see sim.Nemesis.CalmSince). It is
// false during setup. Checks that need a healthy cluster use it rather
// than tracking the nemesis's events.
func (x *Ctx) CalmSince() (time.Time, bool) {
	if x.c.nm == nil {
		return time.Time{}, false
	}
	return x.c.nm.CalmSince()
}

// Logf logs a line with the command's name and the simulated time.
func (x *Ctx) Logf(format string, args ...any) {
	x.c.t.Logf("%s %s %s: %s", time.Now().UTC().Format("15:04:05.000"), kindNames[x.cmd.kind], x.cmd.name, fmt.Sprintf(format, args...))
}

// Until calls f every 100ms until it returns nil, timeout passes, or the
// context is done, and returns f's last error. Recovery checks use it to
// give the system time to heal.
func (x *Ctx) Until(timeout time.Duration, f func() error) error {
	end := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil || !time.Now().Before(end) || x.ctx.Err() != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// of returns the registered commands of kind k.
func (c *Composer) of(k kind) []*command {
	var out []*command
	for _, cmd := range c.cmds {
		if cmd.kind == k {
			out = append(out, cmd)
		}
	}
	return out
}

// check fails the test on a configuration that would test little or
// wrongly.
func (c *Composer) check() {
	t := c.t
	names := map[string]bool{}
	for _, cmd := range c.cmds {
		if names[cmd.name] {
			t.Fatalf("compose: two commands are named %q", cmd.name)
		}
		names[cmd.name] = true
	}
	if len(c.of(parallel))+len(c.of(serial))+len(c.of(singleton))+len(c.of(anytime)) == 0 {
		t.Fatalf("compose: no driver or anytime command")
	}
	if c.cfg.Budget < 20*time.Second {
		t.Fatalf("compose: Budget %v leaves no room for storms and calms; use at least 20s", c.cfg.Budget)
	}
}

// Run runs one timeline. Call it inside the synctest bubble of w, from the
// test's goroutine.
func (c *Composer) Run(t *testing.T) {
	c.t = t
	c.check()
	c.fin = make(chan *run, 64)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	defer func() {
		// After kill, a goroutine frozen with the client node may
		// hold the context's lock, as when it froze removing a child
		// context, so cancel only a context that is not yet canceled,
		// which Err reports without the lock.
		if c.ctx.Err() == nil {
			c.cancel()
		}
	}()
	c.client = c.w.AddNode("client", clientIP)
	c.client.Start(func() {})

	// Setup, without faults.
	c.w.SetCalm(true)
	if firsts := c.of(first); len(firsts) > 0 {
		cmd := firsts[sim.Intn(len(firsts))]
		if !c.wait(cmd, context.Background()) {
			c.report()
			return
		}
	}
	c.w.SetCalm(false)
	lifecycle.SetupComplete(nil)

	// Drive.
	cfg := c.cfg.Faults
	cfg.Exclude = append(slices.Clone(cfg.Exclude), c.client)
	cfg.Event = func(ev sim.Event) {
		c.observe(ev)
		if c.cfg.Faults.Event != nil {
			c.cfg.Faults.Event(ev)
		}
	}
	c.nm = c.w.NewNemesis(cfg)
	c.nm.Start()
	c.drive()

	// Terminal.
	c.terminal()
	c.report()
}

// observe tracks the nemesis's active faults and storms.
func (c *Composer) observe(ev sim.Event) {
	switch ev.Kind {
	case sim.PhaseStorm:
		c.storms++
		return
	case sim.PhaseCalm:
		return
	}
	i := slices.Index(faultKinds, ev.Kind)
	if i < 0 {
		return
	}
	if ev.Start {
		c.active[i]++
	} else if c.active[i] > 0 {
		c.active[i]--
	}
	if ev.Start {
		c.feedback()
	}
}

var faultKinds = []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultKill, sim.FaultKillProcess, sim.FaultStop, sim.FaultPause, sim.FaultThrottle, sim.FaultClockSkip}

// feedback reports to a guided search which commands run alongside which
// kinds of faults, so that a new overlap, such as a membership change
// during a node kill, is new.
func (c *Composer) feedback() {
	var cmds, faults uint64
	for _, r := range c.running {
		if r != nil { // the nemesis's goroutine may see c.running mid-update
			cmds |= 1 << (r.cmd.index % 48)
		}
	}
	for i, n := range c.active {
		if n > 0 {
			faults |= 1 << i
		}
	}
	guide.State("compose", cmds<<16|faults)
}

// start runs cmd on the client node in a new goroutine, which sends its
// run to c.fin when the command returns.
func (c *Composer) start(cmd *command, ctx context.Context) *run {
	r := &run{cmd: cmd}
	c.counts[cmd.name]++
	c.running = append(c.running, r)
	c.client.Go(func() {
		defer func() {
			if p := recover(); p != nil {
				r.panic, r.stack = p, debug.Stack()
			}
			c.fin <- r
		}()
		r.err = cmd.f(&Ctx{c: c, cmd: cmd, ctx: ctx})
	})
	c.feedback()
	return r
}

// finished records a run that returned. A run that panicked panics the
// composer's goroutine with the same value, with the command's stack.
func (c *Composer) finished(r *run) {
	if r.panic != nil {
		fmt.Fprintf(os.Stderr, "compose: %s %s panicked: %v\n\n%s\n", kindNames[r.cmd.kind], r.cmd.name, r.panic, r.stack)
		panic(r.panic)
	}
	c.running = slices.DeleteFunc(c.running, func(x *run) bool { return x == r })
	assert.Always(r.err == nil, "compose: "+kindNames[r.cmd.kind]+" "+r.cmd.name+" succeeds", map[string]any{"err": fmt.Sprint(r.err)})
	if r.err != nil {
		c.failures = append(c.failures, fmt.Sprintf("%s %s: %v", kindNames[r.cmd.kind], r.cmd.name, r.err))
	}
}

// wait runs cmd and waits for it, and reports whether it succeeded.
func (c *Composer) wait(cmd *command, ctx context.Context) bool {
	r := c.start(cmd, ctx)
	for {
		x := <-c.fin
		c.finished(x)
		if x == r {
			return r.err == nil
		}
	}
}

// drive runs the drive phase.
func (c *Composer) drive() {
	length := func() time.Duration {
		// Log-uniform, so short timelines are as common as long ones.
		lo, hi := math.Log(float64(min(15*time.Second, c.cfg.Budget/2))), math.Log(float64(c.cfg.Budget))
		u := float64(sim.Rand()>>11) / (1 << 53)
		return time.Duration(math.Exp(lo + u*(hi-lo)))
	}
	end, branches := time.Now().Add(length()), lifecycle.Branches()
	maxParallel := 1 + sim.Intn(8)
	gap := logUniform(10*time.Millisecond, time.Second)
	pars, sers, anys := c.of(parallel), c.of(serial), c.of(anytime)
	var single *run
	if sings := c.of(singleton); len(sings) > 0 && (len(pars)+len(sers) == 0 || sim.Intn(2) == 0) {
		assert.Reachable("compose: a singleton timeline", nil)
		single = c.start(sings[sim.Intn(len(sings))], c.ctx)
		pars, sers = nil, nil
	}
	var serialWant *command // waits for the parallel drivers to return
	var serialRun *run
	countKind := func(k kind) int {
		n := 0
		for _, r := range c.running {
			if r.cmd.kind == k {
				n++
			}
		}
		return n
	}
	next := time.Now().Add(exp(gap))
	branchGap := logUniform(2*time.Second, 30*time.Second)
	nextBranch := time.Now().Add(exp(branchGap))
	for {
		now := time.Now()
		if b := lifecycle.Branches(); b != branches {
			end, branches = now.Add(length()), b
		}
		if !now.Before(end) || single != nil && !slices.Contains(c.running, single) {
			return
		}
		if !now.Before(nextBranch) {
			nextBranch = now.Add(exp(branchGap))
			// A timeline that failed already would fail every branch.
			if c.t.Failed() {
				continue
			}
			c.branches++
			if sim.Branch("compose: every terminal branch passes") {
				// A branch's output shows only if it fails.
				c.branch = true
				n := sim.BranchNumber()
				c.t.Logf("compose: branch %d runs the terminal phase; to replay it in one process, as under a debugger, run again with GODETBRANCH=%d added to the environment", n, n)
				return
			}
		}
		if serialWant != nil && countKind(parallel) == 0 {
			assert.Reachable("compose: a serial driver runs", nil)
			serialRun, serialWant = c.start(serialWant, c.ctx), nil
		}
		wake := next
		if end.Before(wake) {
			wake = end
		}
		if nextBranch.Before(wake) {
			wake = nextBranch
		}
		select {
		case r := <-c.fin:
			c.finished(r)
			if r == serialRun {
				serialRun = nil
			}
			continue
		case <-time.After(time.Until(wake)):
		}
		if !time.Now().Before(nextBranch) {
			continue
		}
		if time.Now().Before(next) {
			continue
		}
		next = time.Now().Add(exp(gap))
		// Pick what to start: a parallel driver four times as often as
		// a serial one or an anytime check.
		var choices []func()
		idle := serialWant == nil && serialRun == nil
		if idle && len(pars) > 0 && countKind(parallel) < maxParallel {
			p := func() {
				c.start(pick(pars), c.ctx)
				c.peak = max(c.peak, countKind(parallel))
			}
			choices = append(choices, p, p, p, p)
		}
		if idle && len(sers) > 0 {
			choices = append(choices, func() { serialWant = pick(sers) })
		}
		var idleAnys []*command
		for _, a := range anys {
			if !slices.ContainsFunc(c.running, func(r *run) bool { return r.cmd == a }) {
				idleAnys = append(idleAnys, a)
			}
		}
		if len(idleAnys) > 0 {
			choices = append(choices, func() { c.start(pick(idleAnys), c.ctx) })
		}
		if len(choices) > 0 {
			choices[sim.Intn(len(choices))]()
		}
	}
}

// terminal runs the terminal phase.
func (c *Composer) terminal() {
	evs := c.of(eventually)
	terms := append(slices.Clone(evs), c.of(finally)...)
	var cmd *command
	if len(terms) > 0 {
		cmd = terms[sim.Intn(len(terms))]
	}
	if cmd != nil && cmd.kind == finally && !c.drain() {
		assert.Reachable("compose: the drivers do not return in time for a finally command", nil)
		c.t.Logf("compose: the drivers did not return within %v, so finally %s does not run", drainTimeout, cmd.name)
		cmd = nil
		if len(evs) > 0 {
			cmd = evs[sim.Intn(len(evs))]
		}
	}
	if cmd == nil || cmd.kind == eventually {
		c.kill()
	}
	if cmd == nil {
		c.stopFaults()
		return
	}
	guide.State("compose.terminal", uint64(cmd.index)<<8|uint64(min(c.storms, 255)))
	if !cmd.faultsOn {
		c.stopFaults()
	}
	c.wait(cmd, context.Background())
	c.stopFaults()
}

// drain waits up to drainTimeout for every running command to return,
// and reports whether they did.
func (c *Composer) drain() bool {
	timeout := time.After(drainTimeout)
	for len(c.running) > 0 {
		select {
		case r := <-c.fin:
			c.finished(r)
		case <-timeout:
			return false
		}
	}
	return true
}

// kill cancels the drivers' context, waits a second for them to return,
// and then kills the client node, freezing what still runs on it, and
// starts it again.
func (c *Composer) kill() {
	c.cancel()
	grace := time.After(time.Second)
	for len(c.running) > 0 {
		select {
		case r := <-c.fin:
			c.finished(r)
			continue
		case <-grace:
		}
		break
	}
	if len(c.running) > 0 {
		assert.Reachable("compose: a driver is killed", nil)
	}
	c.running = nil
	c.client.KillProcess()
	c.client.Start(func() {})
}

func (c *Composer) stopFaults() {
	if !c.stopped {
		c.stopped = true
		c.nm.Stop()
	}
}

// report logs a summary of the timeline and fails the test if a command
// failed.
func (c *Composer) report() {
	var names []string
	for name, n := range c.counts {
		names = append(names, fmt.Sprintf("%s %d", name, n))
	}
	slices.Sort(names)
	where := "the timeline"
	if c.branch {
		where = fmt.Sprintf("branch %d", sim.BranchNumber())
	}
	c.t.Logf("compose: %s: %d storms; at most %d parallel drivers at once; %d branches; runs: %s", where, c.storms, c.peak, c.branches, strings.Join(names, ", "))
	for _, f := range c.failures {
		c.t.Errorf("compose: %s", f)
	}
}

// pick picks one of cmds by weight, from the fault stream.
func pick(cmds []*command) *command {
	total := 0
	for _, cmd := range cmds {
		total += cmd.weight
	}
	r := sim.Intn(total)
	for _, cmd := range cmds {
		if r -= cmd.weight; r < 0 {
			return cmd
		}
	}
	return cmds[len(cmds)-1]
}

// exp returns an exponentially distributed duration with the given mean,
// from the fault stream.
func exp(mean time.Duration) time.Duration {
	u := float64(sim.Rand()&(1<<26-1)+1) / (1 << 26)
	return time.Duration(-math.Log(u) * float64(mean))
}

// logUniform returns a duration between lo and hi whose logarithm is
// uniformly distributed, from the fault stream.
func logUniform(lo, hi time.Duration) time.Duration {
	u := float64(sim.Rand()&(1<<53-1)) / (1 << 53)
	return time.Duration(float64(lo) * math.Exp(u*math.Log(float64(hi)/float64(lo))))
}
