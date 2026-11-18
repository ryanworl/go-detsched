// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The search.
//
// Every run is a launch, which starts in one of three ways, its arm:
//
//   - fresh: a new seed. A program that reports the end of its setup
//     (detsched/sdk/lifecycle) runs setup in a few roots, fork servers
//     stopped right after it, and each fresh launch forks one with a new
//     branch seed, which reseeds everything after setup; a program that
//     doesn't runs each fresh seed from its beginning, without forking.
//   - cell: a fork of a cell, a state some run reached first (Go-Explore).
//   - directed: a new seed, run from its beginning, whose goroutines yield
//     while they hold a mutex at a site where the lock order predicts a
//     deadlock (DeadlockFuzzer), once some run predicted one.
//
// Every launch runs to its end. A run that recorded a feature no earlier
// run had becomes a cell: the state right after the first such feature.
// Since runs are deterministic, a cell is a record, not a process: its
// root's seed, yield sites and tape, and the switch to stop at, or, for a
// run that did not fork, the features it found first. To launch from a
// cell, the search re-runs it under a fork server to that state and forks
// from the stopped program, which it keeps while the search uses it, in a
// few programs at most.
//
// The search spends its runs on the arms in proportion to what each found
// lately per second of running: new cells, and ten times as much for a new
// failure. Each arm gets at least a tenth of the runs, so that an arm that
// stopped finding things is tried again. It picks cells the same way, by
// what launches from each found per second.
//
// Feedback features are the program's own guide states and assertions
// (detsched/sdk/guide and sdk/assert) if the base run reported a guide
// state, else the runtime's (where goroutines park, select cases, lock
// orders, recovered panics, maximums, and assertions).

const (
	armFresh = iota
	armCell
	armDirected
	numArms
)

var armNames = [numArms]string{"fresh", "cell", "directed"}

const (
	maxCells   = 1024 // cells the archive keeps; the least productive goes first
	liveCells  = 8    // stopped programs of cells kept at once
	setupRoots = 4    // roots stopped after setup that fresh launches fork
	rootForks  = 64   // fresh launches a setup root serves before a new seed replaces it
)

// An arm's statistics, decayed so that recent launches count most.
type arm struct {
	reward, secs float64
	launches     int
}

func (a *arm) rate() float64 { return (a.reward + 1) / (a.secs + 1) }

// A cell is a state a run reached first, named by the run that reached
// it: re-running seed with yield and tape, in fork mode, to switch at
// stops there. A cell from a run that did not fork has at 0 and novel,
// the features it recorded first, instead: its program forks a child
// that goes on as the run did and stops after the first of them.
type cell struct {
	seed  uint64
	yield string
	tape  string
	at    uint64
	novel []int

	chosen int     // launches from it
	found  float64 // what they found: new cells and new failures
	secs   float64 // their running time, and the time to stop its program
	proc   *proc   // its stopped program, while kept
	pinned bool    // a setup root, kept while fresh launches fork it
}

func (c *cell) weight() float64 { return (c.found + 1) / (c.secs + 1) }

// A proc is a program stopped by its fork server, which launches fork:
// a setup root or a cell's program.
type proc struct {
	mu     sync.Mutex // serializes commands to srv
	srv    *forkServer
	pid    int
	tape   string    // the branch points the stopped program applied
	parent *proc     // the server that forked it, or nil
	ready  chan bool // closed once srv is stopped, or failed with err
	err    error
	busy   int       // launches forking it
	used   time.Time // the last launch it served
	closed bool
}

// feedbackKinds are the runtime's feedback feature kinds, in its order
// (runtime/detfeed.go); each has 1<<feedbackBits counters at
// kind<<feedbackBits, except the last, guide, which has the rest of the
// map.
var feedbackKinds = []string{"park", "select", "comm", "lockorder", "recover", "max", "assert", "cover", "guide"}

const feedbackBits = 13

func feedbackKind(i int) string {
	return feedbackKinds[min(i>>feedbackBits, len(feedbackKinds)-1)]
}

// prepare runs the base run, seed 1, twice, and sets up the search from
// it: the root switch, the switch limit, the feedback kinds that count,
// and the seen map.
func (s *search) prepare(ctx context.Context) error {
	feedback := s.temp("base.feedback")
	defer os.Remove(feedback)
	// The third run checks that the base run needs no variables of our
	// environment that a new shell lacks.
	var runs [3]*result
	var errs [3]error
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := s.plain(1, addSearch(nil, "feedback=1")...)
			switch i {
			case 0:
				cmd = s.plain(1, addSearch(nil, "feedback="+feedback)...)
			case 2:
				cmd = s.shellRun(nil)
			}
			runs[i], errs[i] = s.exec(ctx, cmd)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return err
	}
	base := runs[0]
	base.seed = 1
	trace := base.trace()
	if trace == "" {
		return fmt.Errorf("%s printed no detsched trace line; build it with a detsched toolchain and GOEXPERIMENT=detsched\n%s", s.bin, truncate(string(base.output), 2000))
	}
	s.baseTrace, s.baseOutput = trace, base.output
	if runs[1].trace() != trace {
		s.unstable = fmt.Sprintf("seed 1 ran two different schedules (%q, then %q), so failures may not replay; the program depends on something besides its seed, such as real time or the OS", trace, runs[1].trace())
		s.printf("detsched: warning: %s\n", s.unstable)
	} else if runs[2].trace() != trace {
		vars, ok, err := s.findVars(ctx, s.shellRun, trace)
		if err != nil {
			return err
		}
		if !ok {
			// The variables found are not enough, so the schedule depends
			// on something else; keep them off replay lines.
			s.printf("detsched: warning: seed 1 runs another schedule in a new shell, so replay lines may not reproduce failures there\n")
		} else {
			s.vars = vars
		}
	}
	if reports := leftReport.FindAll(base.output, -1); len(reports) > 0 {
		s.left = len(reports)
		var b strings.Builder
		b.WriteString("detsched: warning: seed 1 left the deterministic subset, so failures may not replay:\n")
		for i, r := range reports {
			if i == 5 {
				fmt.Fprintf(&b, "\t(and %d more)\n", len(reports)-i)
				break
			}
			b.WriteString("\t" + strings.ReplaceAll(strings.TrimSuffix(string(r), "\n"), "\n", "\n\t") + "\n")
		}
		s.printf("%s", b.String())
	}
	// A run that makes 1000 times the switches of seed 1 spins, and is
	// stopped. Seed 1 measures a whole run only if it passed: one that
	// failed early would stop runs that are only longer, so then -timeout
	// alone stops them.
	if !base.failed() {
		m := switchesField.FindStringSubmatch(trace)
		s.maxSwitches = 1000 * max(atou(m[1]), 1000)
	}
	if m := setupLine.FindSubmatch(base.output); m != nil {
		s.setup, s.stop = true, atou(string(m[1]))+1
	}
	s.predict(base.output)

	hits, err := os.ReadFile(feedback)
	if err != nil {
		return err
	}
	counted := map[string]bool{"assert": true}
	if k := slices.Index(feedbackKinds, "guide"); slices.ContainsFunc(hits[k<<feedbackBits:], func(b byte) bool { return b != 0 }) {
		counted["guide"] = true
		s.novelty = "the program's guide states and assertions"
	} else {
		for _, k := range []string{"park", "select", "lockorder", "recover", "max"} {
			counted[k] = true
		}
		s.novelty = "the runtime's features, as the program reports no guide states (see the README's \"The search\")"
	}
	f, err := os.Create(s.temp("seen"))
	if err != nil {
		return err
	}
	s.seenFile = f
	if err := f.Truncate(int64(len(hits))); err != nil {
		return err
	}
	s.seen, err = syscall.Mmap(int(f.Fd()), 0, len(hits), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	for i := range s.seen {
		if hits[i] != 0 || !counted[feedbackKind(i)] {
			s.seen[i] = 1
		}
	}
	s.canFork = !s.noFork
	return s.record(ctx, base, armFresh)
}

var addrs = regexp.MustCompile(`0x[0-9a-f]+`)

// predict adds the deadlock directives in a run's output to the sites
// directed launches yield at.
func (s *search) predict(output []byte) {
	cycles := deadlockLine.FindAllSubmatch(output, -1)
	for i, m := range directiveLine.FindAllSubmatch(output, -1) {
		added := false
		for _, site := range strings.Split(string(m[1]), ",") {
			if !slices.Contains(s.yieldSites, site) && len(s.yieldSites) < 16 {
				s.yieldSites = append(s.yieldSites, site)
				added = true
			}
		}
		if added && i < len(cycles) {
			p := addrs.ReplaceAllString(string(cycles[i][1]), "0x?")
			if !slices.Contains(s.predicted, p) {
				s.predicted = append(s.predicted, p)
			}
		}
	}
}

// pickArm picks the arm of the next launch. s.mu must be held.
func (s *search) pickArm() int {
	if !s.canFork {
		return armFresh
	}
	if s.only == armFresh || s.only == armCell && len(s.cells) > 0 || s.only == armDirected && len(s.yieldSites) > 0 {
		return s.only
	}
	var w [numArms]float64
	var sum float64
	for i := range s.arms {
		if i == armCell && len(s.cells) == 0 || i == armDirected && len(s.yieldSites) == 0 {
			continue
		}
		w[i] = s.arms[i].rate()
		sum += w[i]
	}
	n := 0
	for _, x := range w {
		if x > 0 {
			n++
		}
	}
	r := s.rng.Float64()
	for i, x := range w {
		if x == 0 {
			continue
		}
		// At least a tenth of the runs each.
		p := 0.1 + (1-0.1*float64(n))*x/sum
		if r -= p; r < 0 {
			return i
		}
	}
	return armFresh
}

// credit records what a launch of arm found in secs of running.
func (s *search) credit(a int, reward float64, secs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.arms {
		s.arms[i].reward *= 0.99
		s.arms[i].secs *= 0.99
	}
	s.arms[a].reward += reward
	s.arms[a].secs += secs
	s.arms[a].launches++
}

// launch runs one launch of the arm s.pickArm picks.
func (s *search) launch(ctx context.Context, seed uint64) error {
	s.mu.Lock()
	a := s.pickArm()
	var c *cell
	if a == armCell {
		c = s.pickCell()
	}
	yield := ""
	if a == armDirected {
		yield = strings.Join(s.yieldSites, ",")
	}
	s.mu.Unlock()
	start := time.Now()
	var reward float64
	var err error
	switch {
	case c != nil:
		reward, err = s.forkLaunch(ctx, armCell, c, seed)
	case a == armFresh && s.setup && s.canFork:
		reward, err = s.rootLaunch(ctx, seed)
	default:
		reward, err = s.plainLaunch(ctx, a, seed, yield)
	}
	if err == nil {
		secs := time.Since(start).Seconds()
		s.credit(a, reward, secs)
		if c != nil {
			s.mu.Lock()
			c.found += reward
			c.secs += secs
			s.mu.Unlock()
		}
	}
	return err
}

// plainLaunch runs seed from its beginning without forking. If the run
// recorded features no run had, it becomes a cell.
func (s *search) plainLaunch(ctx context.Context, a int, seed uint64, yield string) (float64, error) {
	fb := s.temp(fmt.Sprintf("plain-%d.feedback", seed))
	defer os.Remove(fb)
	r, err := s.exec(ctx, s.plain(seed, addSearch(nil, append(yieldField(yield), "feedback="+fb)...)...))
	if err != nil {
		return 0, err
	}
	r.seed, r.yield = seed, yield
	cells := 0
	if hits, _ := os.ReadFile(fb); !r.failed() && s.canFork {
		if novel := s.claim(hits); len(novel) > 0 {
			s.addCell(&cell{seed: seed, yield: yield, novel: novel})
			cells++
		}
	}
	return s.reward(cells, r), s.record(ctx, r, a)
}

// rootLaunch forks a setup root with branch seed seed.
func (s *search) rootLaunch(ctx context.Context, seed uint64) (float64, error) {
	s.mu.Lock()
	var root *cell
	for i := 0; i < len(s.pool); i++ {
		if c := s.pool[i]; c.chosen >= rootForks {
			// Retired: the cache of stopped programs closes it once idle.
			s.pool = slices.Delete(s.pool, i, i+1)
			i--
			if c.proc != nil {
				s.lru = append(s.lru, c)
			}
		} else if root == nil || c.chosen < root.chosen {
			root = c
		}
	}
	if len(s.pool) < setupRoots {
		root = &cell{seed: seed, at: s.stop, pinned: true}
		s.pool = append(s.pool, root)
	}
	root.chosen++
	s.mu.Unlock()
	return s.forkLaunch(ctx, armFresh, root, seed)
}

// forkLaunch stops c's program, unless kept, and forks it with branch
// seed seed. If c's program ends before its state, or refuses to fork,
// the search drops c and runs seed without forking instead.
func (s *search) forkLaunch(ctx context.Context, a int, c *cell, seed uint64) (float64, error) {
	p, err := s.materialize(ctx, c)
	if err == nil {
		var r *result
		var at uint64
		r, at, err = s.forkRun(ctx, p, c, seed)
		s.release(p)
		if err == nil {
			cells := 0
			if at > 0 && !r.failed() {
				s.addCell(&cell{seed: c.seed, yield: c.yield, tape: r.tape, at: at})
				cells++
			}
			return s.reward(cells, r), s.record(ctx, r, a)
		}
	}
	if !errors.Is(err, errGone) && !s.isRefused(err, c.pinned) {
		return 0, err
	}
	s.dropCell(c)
	return s.plainLaunch(ctx, a, seed, "")
}

// errGone reports a cell whose program ended before it reached the cell.
var errGone = errors.New("the program ended before the cell")

// isRefused reports whether err is a refusal to fork. A setup root that
// refuses 10 times before any fork succeeded, or a refusal for cgo or
// open files, which later forks would get too, makes the search stop
// forking.
func (s *search) isRefused(err error, root bool) bool {
	var refused errRefused
	if !errors.As(err, &refused) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused++
	if (root && !s.forked && s.refused >= 10 || refused.reason == forkCgo || refused.reason == forkFiles) && s.canFork {
		s.canFork = false
		s.refusal = refused.Error()
		s.printf("detsched: the program refused to fork (%s), so the search runs fresh seeds without forking\n", s.refusal)
	}
	return true
}

// claim returns the features in a plain run's hits that no run recorded
// and no cell claims, and claims them for the run's cell. Only the one its
// program stops after counts as seen, once the search stops it; the others
// stay new, for the runs from the cell to reach and become cells there.
func (s *search) claim(hits []byte) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(hits) < len(s.seen) {
		return nil
	}
	var novel []int
	for i, h := range hits[:len(s.seen)] {
		if h != 0 && s.seen[i] == 0 && !s.claimed[i] {
			s.claimed[i] = true
			novel = append(novel, i)
		}
	}
	return novel
}

// see marks feature i seen.
func (s *search) see(i int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[i] == 0 {
		s.seen[i] = 1
		s.features++
	}
}

// addCell adds c to the archive, removing the least productive cell
// without a stopped program when it is full.
func (s *search) addCell(c *cell) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cells) >= maxCells {
		ei := -1
		for i, o := range s.cells {
			if o.proc == nil && (ei < 0 || o.weight() < s.cells[ei].weight()) {
				ei = i
			}
		}
		if ei < 0 {
			return
		}
		s.cells = slices.Delete(s.cells, ei, ei+1)
	}
	s.cells = append(s.cells, c)
}

// dropCell removes c from the archive and the setup roots.
func (s *search) dropCell(c *cell) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range c.novel {
		delete(s.claimed, i)
	}
	if i := slices.Index(s.cells, c); i >= 0 {
		s.cells = slices.Delete(s.cells, i, i+1)
	}
	if i := slices.Index(s.pool, c); i >= 0 {
		s.pool = slices.Delete(s.pool, i, i+1)
	}
}

// detBranchGoesOn marks a branch seed that goes on with the run's own
// random choices (runtime/detsched.go).
const detBranchGoesOn = 1 << 63

// yieldField returns the GODETSEARCH field for the yield sites, if any.
func yieldField(yield string) []string {
	if yield == "" {
		return nil
	}
	return []string{"yield=" + yield}
}

// reward returns what a launch that made cells new cells and ended in r is
// worth: one for each cell, and ten for a new failure signature.
func (s *search) reward(cells int, r *result) float64 {
	x := float64(cells)
	if r != nil && r.failed() && !r.tooLong() {
		s.mu.Lock()
		if s.byKey[r.key()] == nil {
			x += 10
		}
		s.mu.Unlock()
	}
	return x
}

// pickCell picks a cell in proportion to what launches from it found per
// second, which favors cells not tried yet. s.mu must be held.
func (s *search) pickCell() *cell {
	var sum float64
	for _, c := range s.cells {
		sum += c.weight()
	}
	r := s.rng.Float64() * sum
	for i, c := range s.cells {
		if r -= c.weight(); r < 0 || i == len(s.cells)-1 {
			c.chosen++
			return c
		}
	}
	panic("unreachable")
}

// materialize returns c's stopped program, stopping one first unless one
// is kept. The caller releases it.
func (s *search) materialize(ctx context.Context, c *cell) (*proc, error) {
	s.mu.Lock()
	if p := c.proc; p != nil {
		p.busy++
		p.used = time.Now()
		s.mu.Unlock()
		<-p.ready
		if p.err != nil {
			s.release(p)
		}
		return p, p.err
	}
	p := &proc{ready: make(chan bool), busy: 1, used: time.Now()}
	c.proc = p
	if !c.pinned {
		s.lru = append(s.lru, c)
	}
	idle := s.trim(liveCells)
	s.mu.Unlock()
	s.closeProcs(idle)

	start := time.Now()
	p.err = s.stopCell(ctx, c, p)
	s.mu.Lock()
	c.secs += time.Since(start).Seconds()
	if p.err != nil {
		c.proc = nil
		if i := slices.Index(s.lru, c); i >= 0 {
			s.lru = slices.Delete(s.lru, i, i+1)
		}
	}
	s.mu.Unlock()
	close(p.ready)
	if p.err != nil {
		s.release(p)
	}
	return p, p.err
}

// stopCell starts c's program under a fork server and stops it at c's
// state, in p.
func (s *search) stopCell(ctx context.Context, c *cell, p *proc) error {
	fields := append(yieldField(c.yield), "feedback=1", fmt.Sprintf("maxswitches=%d", s.maxSwitches))
	if c.tape != "" {
		fields = append(fields, "tape="+c.tape)
	}
	cmd := s.plain(c.seed, addSearch(nil, fields...)...)
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdout, cmd.Stderr = devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	srv, err := startForkServer(cmd)
	if err != nil {
		return err
	}
	pid := cmd.Process.Pid
	s.mu.Lock()
	s.roots[pid] = true
	s.mu.Unlock()
	root := &proc{srv: srv, pid: pid}
	p.srv, p.pid, p.tape = srv, pid, c.tape
	if c.at > 0 {
		ok, err := s.runSrv(ctx, srv, pid, start, func() (bool, error) { return srv.run(c.at) })
		if err == errHung {
			err = errCanceled // only a run to a cell, whose output is not kept
		}
		if err == nil && !ok {
			err = errGone
		}
		if err != nil {
			s.closeProc(root)
		}
		return err
	}

	// A run that did not fork: fork a child that goes on as the run did,
	// with the features the run found first missing from its seen map,
	// so that it stops after the first of them.
	seen, err := os.CreateTemp(s.out, "seen")
	if err != nil {
		s.closeProc(root)
		return err
	}
	defer os.Remove(seen.Name())
	defer seen.Close()
	s.mu.Lock()
	b := slices.Clone(s.seen)
	s.mu.Unlock()
	for _, i := range c.novel {
		b[i] = 0
	}
	if _, err := seen.Write(b); err != nil {
		s.closeProc(root)
		return err
	}
	bseed := c.seed | detBranchGoesOn
	cpid, child, err := srv.branch(bseed, 0, branchFiles{stdout: devnull, stderr: devnull, control: true, seen: seen})
	if err != nil {
		s.closeProc(root)
		return err
	}
	p.srv, p.pid, p.parent = child, cpid, root
	p.tape = fmt.Sprintf("%d:%d", srv.at, bseed)
	var feature int
	ok, err := s.runSrv(ctx, child, cpid, start, func() (bool, error) {
		ok, f, err := child.runNovel()
		feature = f
		return ok, err
	})
	if err == errHung {
		err = errCanceled
	}
	if err == nil && !ok {
		err = errGone
	}
	if err != nil {
		s.closeProc(p)
		return err
	}
	// Later, re-run it to the switch instead.
	s.see(feature)
	s.mu.Lock()
	for _, i := range c.novel {
		delete(s.claimed, i)
	}
	c.tape, c.at, c.novel = p.tape, child.at, nil
	s.mu.Unlock()
	return nil
}

// release ends a launch's use of p, and closes stopped programs the
// search no longer keeps.
func (s *search) release(p *proc) {
	s.mu.Lock()
	p.busy--
	idle := s.trim(liveCells)
	s.mu.Unlock()
	s.closeProcs(idle)
}

// trim removes the least recently used idle programs from the cache
// until it holds at most n, and returns them, to close after unlocking
// s.mu. s.mu must be held.
func (s *search) trim(n int) []*proc {
	var idle []*proc
	for len(s.lru) > n {
		li := -1
		for i, c := range s.lru {
			if p := c.proc; p != nil && p.busy == 0 && (li < 0 || p.used.Before(s.lru[li].proc.used)) {
				li = i
			}
		}
		if li < 0 {
			break
		}
		idle = append(idle, s.lru[li].proc)
		s.lru[li].proc = nil
		s.lru = slices.Delete(s.lru, li, li+1)
	}
	return idle
}

func (s *search) closeProcs(ps []*proc) {
	for _, p := range ps {
		s.closeProc(p)
	}
}

// closeProc ends p's program, and the root that forked it, if any.
func (s *search) closeProc(p *proc) {
	s.mu.Lock()
	closed := p.closed
	p.closed = true
	s.mu.Unlock()
	if closed || p.srv == nil {
		return
	}
	p.srv.close()
	if p.parent != nil {
		p.parent.mu.Lock()
		p.parent.srv.wait(p.pid)
		p.parent.mu.Unlock()
		s.closeProc(p.parent)
		return
	}
	s.mu.Lock()
	delete(s.roots, p.pid)
	s.mu.Unlock()
}

// forkRun forks c's stopped program p with branch seed bseed, runs the
// child to its end, and returns its result, and the switch right after
// its first feature no run had recorded, or 0.
func (s *search) forkRun(ctx context.Context, p *proc, c *cell, bseed uint64) (*result, uint64, error) {
	out, err := os.CreateTemp(s.out, "child")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	fb, err := os.CreateTemp(s.out, "feedback")
	if err != nil {
		return nil, 0, err
	}
	defer os.Remove(fb.Name())
	defer fb.Close()
	start := time.Now()
	p.mu.Lock()
	at := p.srv.at
	pid, child, err := p.srv.branch(bseed, 0, branchFiles{stdout: out, stderr: out, control: true, feedback: fb, seen: s.seenFile})
	p.mu.Unlock()
	if err != nil {
		return nil, 0, err // including errRefused, which branch does not wrap
	}
	s.mu.Lock()
	s.forked = true
	s.mu.Unlock()
	untrack := s.track(start, pid, func() { syscall.Kill(pid, syscall.SIGKILL) })
	defer untrack()
	tape := fmt.Sprintf("%d:%d", at, bseed)
	if p.tape != "" {
		tape = p.tape + "," + tape
	}
	_, err = s.runSrv(ctx, child, pid, start, func() (bool, error) { return child.run(0) })
	hung := err == errHung
	timedOut := errors.Is(err, os.ErrDeadlineExceeded) || hung
	child.close()
	p.mu.Lock()
	_, werr := p.srv.wait(pid)
	p.mu.Unlock()
	if err != nil && !timedOut {
		return nil, 0, fmt.Errorf("running a fork: %w", err)
	}
	if werr != nil {
		return nil, 0, fmt.Errorf("collecting a fork: %w", werr)
	}
	hits, _ := os.ReadFile(fb.Name())
	output, size := readOutput(out.Name())
	r := &result{seed: c.seed, yield: c.yield, tape: tape, output: output, outputSize: size, exitCode: -1, timedOut: timedOut, hung: hung, wall: time.Since(start)}
	r.outOfMem, r.memAtStop = untrack()
	var novelAt uint64
	if len(hits) == len(s.seen)+32 {
		r.exitCode = int(int64(binary.LittleEndian.Uint64(hits[len(s.seen):])))
		if f := binary.LittleEndian.Uint64(hits[len(s.seen)+8:]); f != 0 {
			// Only the first new feature counts as seen: the run becomes
			// a cell right after it, and runs from there reach the rest.
			novelAt = binary.LittleEndian.Uint64(hits[len(s.seen)+16:]) + 1
			s.see(int(f - 1))
			r.newFeatures = 1
		}
	} else if !timedOut {
		r.exitCode = 2 // killed by a signal before it wrote its feedback
	}
	return r, novelAt, nil
}

// waitMem waits while memory is low, closing idle stopped programs.
func (s *search) waitMem(ctx context.Context) {
	start := time.Now()
	for ctx.Err() == nil {
		s.mu.Lock()
		if !s.memLow() {
			s.mu.Unlock()
			return
		}
		if time.Since(start) > 5*time.Second && time.Since(s.memWaited) > 30*time.Second {
			s.memWaited = time.Now()
			avail, total := memory()
			s.printf("detsched: waiting to start runs until a quarter of the memory is available: %d of %d MB\n", avail>>20, total>>20)
		}
		idle := s.trim(0)
		s.mu.Unlock()
		if len(idle) == 0 {
			time.Sleep(100 * time.Millisecond)
		}
		s.closeProcs(idle)
	}
}

// closeAll ends every stopped program and kills what is left of the
// roots' process groups.
func (s *search) closeAll() {
	s.mu.Lock()
	s.kept = len(s.cells)
	var ps []*proc
	for _, c := range append(slices.Clone(s.lru), s.pool...) {
		if c.proc != nil {
			ps = append(ps, c.proc)
			c.proc = nil
		}
	}
	s.lru, s.pool = nil, nil
	s.mu.Unlock()
	s.closeProcs(ps)
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid := range s.roots {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
	if s.seen != nil {
		syscall.Munmap(s.seen)
		s.seenFile.Close()
	}
}

// memLow reports whether less than a quarter of the memory is available,
// and reports it the first time. A run may take much more memory than the
// base run. s.mu must be held.
func (s *search) memLow() bool {
	avail, total := memory()
	low := avail < total/4
	if low && !s.lowMem {
		s.lowMem = true
		var self runtime.MemStats
		runtime.ReadMemStats(&self)
		s.printf("detsched: memory is low (%d of %d MB available, under a quarter; detsched itself holds %d MB, and the runs' output is up to %d MB each), so the search keeps fewer stopped programs and starts fewer runs\n", avail>>20, total>>20, self.Sys>>20, s.bigOutput>>20)
	}
	return low
}

// A liveRun is a running program, process pid, which guardMem may stop.
type liveRun struct {
	start   time.Time
	pid     int
	kill    func()
	stopped bool
	mem     uint64 // the private memory it held when guardMem stopped it
}

// track records a running program, process pid, that kill stops, until
// the returned function is called, which reports whether guardMem stopped
// it and how much private memory it held then.
func (s *search) track(start time.Time, pid int, kill func()) (untrack func() (bool, uint64)) {
	l := &liveRun{start: start, pid: pid, kill: kill}
	s.mu.Lock()
	s.running[l] = true
	s.mu.Unlock()
	return func() (bool, uint64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.running, l)
		return l.stopped, l.mem
	}
}

// guardMem stops the run that holds the most private memory, or where
// that is unknown the one that has run longest, whenever less than a
// tenth of the memory is available, until ctx is done, so that the search
// cannot run the machine out of memory: a run that takes much more memory
// than others, or many kept cells at once. A forked run shares most of its
// pages with the program it was forked from, so stopping the oldest would
// free little while a runaway run went on growing.
func (s *search) guardMem(ctx context.Context) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if avail, total := memory(); avail >= total/10 {
			continue
		}
		s.mu.Lock()
		var pick *liveRun
		var pickMem uint64
		for l := range s.running {
			if l.stopped {
				continue
			}
			m := privateMem(l.pid)
			if pick == nil || m > pickMem || m == pickMem && l.start.Before(pick.start) {
				pick, pickMem = l, m
			}
		}
		if pick != nil {
			pick.stopped, pick.mem = true, pickMem
			pick.kill()
		}
		s.mu.Unlock()
	}
}

// errHung is runSrv's error for a run the search's end stopped after
// mayHang said it may hang.
var errHung = errors.New("still running when the search ended")

// runSrv runs f, which waits for a reply from srv, within the run's
// timeout from start, and stops process pid when it passes or ctx is
// done: with SIGQUIT, for its stacks, after the timeout or when the run
// may hang (errHung), else with SIGKILL. It then clears the deadline,
// since srv may be kept and serve other runs.
func (s *search) runSrv(ctx context.Context, srv *forkServer, pid int, start time.Time, f func() (bool, error)) (bool, error) {
	srv.conn.SetReadDeadline(start.Add(s.timeout))
	stop := context.AfterFunc(ctx, func() { srv.conn.SetReadDeadline(time.Now()) })
	ok, err := f()
	if !stop() {
		if !s.mayHang(start) {
			syscall.Kill(pid, syscall.SIGKILL)
			return false, errCanceled
		}
		err = errHung
	}
	srv.conn.SetReadDeadline(time.Time{})
	if errors.Is(err, os.ErrDeadlineExceeded) || err == errHung {
		syscall.Kill(pid, syscall.SIGQUIT)
		go func() {
			time.Sleep(10 * time.Second)
			syscall.Kill(pid, syscall.SIGKILL)
		}()
	}
	return ok, err
}

// plain returns a command that runs seed without fork mode, with env added.
// It runs in the package's directory with PWD set to it, as the cd of a
// replay line sets it, since os.Getwd reads PWD.
func (s *search) plain(seed uint64, env ...string) *exec.Cmd {
	cmd := exec.Command(s.bin, s.args...)
	cmd.Dir = s.dir
	cmd.Env = append(append(os.Environ(), s.env...), "PWD="+s.dir, fmt.Sprintf("GODETSEED=%d", seed))
	cmd.Env = addSearch(append(cmd.Env, env...), "envread")
	return cmd
}
