// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheck checks that each corpus program inside the deterministic
// subset runs the same way in every run with a seed (the same schedule,
// output, and exit status), and that the seed picks what a program sees of
// the runtime's randomness: for those programs another seed gives another
// output.
func TestCheck(t *testing.T) {
	cfg := config{runs: 4, parallel: 4}
	for _, tc := range []struct {
		pkg    string
		seeded bool // GODETSEED=1 and 2 give different output
	}{
		{"hello", false},       // one goroutine and the runtime's own
		{"maporder", true},     // map iteration order
		{"selectorder", true},  // select between ready channels
		{"cryptorand", true},   // crypto/rand and crypto's own randomness
		{"shuffle", true},      // -test.shuffle=on
		{"gcheap", true},       // GC timing, cleanups, heap addresses
		{"gcstress", true},     // concurrent allocation through many GCs
		{"gcidle", false},      // waiting for finalizers and cleanups without runtime.GC
		{"mutexstarve", false}, // sync.Mutex starvation mode
		{"synctest", false},    // timers in a bubble, a sleep outside it
		{"nonidle", false},     // bubbles blocked where synctest does not count it
	} {
		bin, cleanup, err := build(cfg, "../../testdata/"+tc.pkg)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		var stdouts, traces []string
		for _, seed := range []string{"1", "2"} {
			outs, _, err := runAll(cfg, bin, 4, func(int) []string { return []string{"GODETSEED=" + seed} })
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range outs[1:] {
				if o != outs[0] {
					t.Errorf("%s with GODETSEED=%s is not deterministic:\n%+v\n%+v", tc.pkg, seed, outs[0], o)
					break
				}
			}
			if outs[0].exitCode != 0 {
				t.Errorf("%s with GODETSEED=%s: exit status %d\n%s%s", tc.pkg, seed, outs[0].exitCode, outs[0].stdout, outs[0].stderr)
			}
			stdouts = append(stdouts, outs[0].stdout)
			traces = append(traces, outs[0].trace)
		}
		if tc.seeded && stdouts[0] == stdouts[1] {
			t.Errorf("%s: GODETSEED=1 and GODETSEED=2 gave the same output %q", tc.pkg, stdouts[0])
		}
		if tc.pkg == "hello" && stdouts[0] != "hello\n" {
			t.Errorf("hello: stdout %q, want %q", stdouts[0], "hello\n")
		}
		if ok, _ := regexp.MatchString(`^trace=0x[0-9a-f]+ switches=[1-9]\d* syncs=\d+$`, traces[0]); !ok {
			t.Errorf("%s: trace %q, want a hash and a nonzero switch count", tc.pkg, traces[0])
		}
	}

	// Tight loops, with and without calls, take turns at deterministic
	// points, and the quantum, which preemptentry below and TestRangeGC
	// set, moves those points.
	var traces []string
	for _, quantum := range []string{"", "20000"} {
		t.Setenv("GODETSEARCH", "perturb=off")
		if quantum != "" {
			t.Setenv("GODETSEARCH", "perturb=off quantum="+quantum)
		}
		r, err := check(cfg, "../../testdata/spin")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Fatalf("spin with quantum=%q is not deterministic:\n%s", quantum, r.details())
		}
		if turns := len(strings.TrimSpace(r.outcomes[0].stdout)); turns < 10 {
			t.Errorf("spin with quantum=%q: goroutines took %d turns, want at least 10:\n%s", quantum, turns, r.details())
		}
		traces = append(traces, r.outcomes[0].trace)
	}
	if traces[0] == traces[1] {
		t.Errorf("spin: quantum=20000 gave the default quantum's schedule %s", traces[0])
	}

	// Preempting at every tick, including at the entry of a function with
	// a defer while GC runs, must not show GC an uninitialized result.
	t.Setenv("GODETSEARCH", "perturb=off quantum=1")
	r, err := check(cfg, "../../testdata/preemptentry")
	if err != nil {
		t.Fatal(err)
	}
	if !r.deterministic() || r.outcomes[0].exitCode != 0 {
		t.Errorf("preemptentry with quantum=1: got %s, want a deterministic pass:\n%s", r.summary(), r.details())
	}
	t.Setenv("GODETSEARCH", "")

	// A program whose output depends on the OS must be reported.
	r, err = check(cfg, "../../testdata/pid")
	if err != nil {
		t.Fatal(err)
	}
	if r.deterministic() || !slices.Contains(r.differences(), "stdout") {
		t.Errorf("pid: got %s, want different stdout across runs", r.summary())
	}
}

// TestExplore checks that scheduling perturbation finds each seeded bug,
// which never shows without perturbation, that the failure reproduces from
// its seed, and that a perturbed run is still deterministic. The seeded
// bugs use the default strategy swarm, which runs PCT for some seeds.
func TestExplore(t *testing.T) {
	cfg := config{runs: 3, parallel: 4}
	for _, pkg := range []string{"../../testdata/bugorder", "../../testdata/bugcache", "../../testdata/buglockorder", "../../testdata/bugatomic"} {
		r, err := check(cfg, pkg)
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() || r.outcomes[0].exitCode != 0 {
			t.Errorf("%s without perturbation: got %s, want a deterministic pass:\n%s", pkg, r.summary(), r.details())
		}
		// How often a seed finds a bug depends on the binary, so on the
		// architecture. bugatomic is the rarest: about 1 in 30 seeds on
		// linux/amd64 and 1 in 40 on darwin/arm64 (first at seed 60).
		seeds := 100
		if strings.HasSuffix(pkg, "bugatomic") {
			seeds = 400
		}
		x, err := exploreSeeds(cfg, pkg, 1, seeds, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(x.failures) == 0 || !x.reproduced {
			t.Errorf("%s: got %s, want a failure that reproduces", pkg, x.report())
		}
	}

	x, err := exploreSeeds(cfg, "../../testdata/nonidle", 1, 10, "walk")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.failures) > 0 || x.schedules < 5 {
		t.Errorf("nonidle: got %s, want no failures and at least 5 distinct schedules", x.report())
	}

	// PCT finds the order violation. With few change points, only the
	// budget per priority keeps a higher-priority spinner from waiting
	// forever.
	x, err = exploreSeeds(cfg, "../../testdata/bugorder", 1, 40, "pct")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.failures) == 0 || !x.reproduced {
		t.Errorf("bugorder under PCT: got %s, want a failure that reproduces", x.report())
	}
	x, err = exploreSeeds(cfg, "../../testdata/spinwait", 1, 20, "pct")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.failures) > 0 {
		t.Errorf("spinwait under PCT: got %s, want no failures", x.report())
	}

	for _, perturb := range []string{"perturb=walk", "perturb=pct", ""} {
		t.Setenv("GODETSEED", "3")
		t.Setenv("GODETSEARCH", perturb)
		cfg.runs = 6
		r, err := check(cfg, "../../testdata/nonidle")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Errorf("nonidle with GODETSEED=3 and %s is not deterministic:\n%s", perturb, r.details())
		}
	}
}

// TestTape checks that a branch point N:S in the tape keeps the run
// identical up to switch N and changes it after, that it also changes
// the program's own random choices, and that a branched run is still
// deterministic.
func TestTape(t *testing.T) {
	cfg := config{runs: 6, parallel: 3}
	switches := func(bin, tape, perturb string) []string {
		cmd := exec.Command(bin)
		search := "perturb=" + perturb
		if tape != "" {
			search += " tape=" + tape
		}
		cmd.Env = append(os.Environ(), "GODETSEED=3", "GODETSEARCH="+search, "GODEBUG=dettrace=2")
		out, _ := cmd.CombinedOutput()
		var lines []string
		for _, l := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(l, "detsched: switch ") {
				lines = append(lines, l)
			}
		}
		return lines
	}
	bin, cleanup, err := build(cfg, "../../testdata/gcstress")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	const n = 20
	for _, godebug := range []string{"walk", "pct"} {
		base, branched := switches(bin, "", godebug), switches(bin, fmt.Sprintf("%d:7", n), godebug)
		if len(base) <= n || len(branched) <= n {
			t.Fatalf("%s: %d and %d switches, want more than %d", godebug, len(base), len(branched), n)
		}
		if !slices.Equal(base[:n], branched[:n]) {
			t.Errorf("%s: switches before the branch point differ", godebug)
		}
		if slices.Equal(base, branched) {
			t.Errorf("%s: a branch at switch %d did not change the schedule", godebug, n)
		}
		// A branch point that goes on (the top bit of its seed, as
		// -guide's continuations) leaves the run as it was.
		if goesOn := switches(bin, fmt.Sprintf("%d:%d", n, uint64(1<<63|7)), godebug); !slices.Equal(base, goesOn) {
			t.Errorf("%s: a branch point that goes on changed the schedule", godebug)
		}
	}

	t.Setenv("GODETSEED", "3")
	var outputs []string
	for _, tape := range []string{"", "tape=2:7"} {
		t.Setenv("GODETSEARCH", tape)
		r, err := check(cfg, "../../testdata/maporder")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Fatalf("maporder with GODETSEARCH=%q is not deterministic:\n%s", tape, r.details())
		}
		outputs = append(outputs, r.outcomes[0].stdout)
	}
	if outputs[0] == outputs[1] {
		t.Errorf("maporder: tape=2:7 did not change the program's random choices")
	}
}

// TestOutcomeDurations checks that outcomes ignore the real time package
// testing prints for each test, so that a failing test binary that runs
// identically compares equal.
func TestOutcomeDurations(t *testing.T) {
	a := newOutcome("--- FAIL: TestX (0.98s)\n    x_test.go:1: bad\n    --- PASS: TestX/sub (0.01s)\nFAIL\n", "", 1)
	b := newOutcome("--- FAIL: TestX (1.02s)\n    x_test.go:1: bad\n    --- PASS: TestX/sub (0.00s)\nFAIL\n", "", 1)
	if a != b {
		t.Errorf("outcomes differ only in test durations, but compare unequal:\n%q\n%q", a.stdout, b.stdout)
	}
	if c := newOutcome("took (0.98s)\n", "", 0); c.stdout != "took (0.98s)\n" {
		t.Errorf("program output changed to %q", c.stdout)
	}
}

// TestSim checks that a cluster on the simulated network and disk
// (testdata/simkv, using package detsched/sdk/sim) runs deterministically,
// that GODETSEED picks its faults (here, message latencies), and that a
// branch point reseeds them, with the schedule left unperturbed.
func TestSim(t *testing.T) {
	cfg := config{runs: 6, parallel: 3}
	var outputs []string
	for _, env := range [][2]string{{"1", ""}, {"2", ""}, {"1", " tape=60:9"}} {
		t.Setenv("GODETSEED", env[0])
		t.Setenv("GODETSEARCH", "perturb=off"+env[1])
		r, err := check(cfg, "../../testdata/simkv")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Fatalf("simkv with GODETSEED=%s%s is not deterministic:\n%s", env[0], env[1], r.details())
		}
		o := r.outcomes[0]
		if o.exitCode != 0 || !strings.Contains(o.stdout, "client: after heal") {
			t.Fatalf("simkv with GODETSEED=%s%s: exit status %d, stdout:\n%s", env[0], env[1], o.exitCode, o.stdout)
		}
		outputs = append(outputs, o.stdout)
	}
	if outputs[0] == outputs[1] {
		t.Errorf("simkv: GODETSEED=1 and 2 gave the same output")
	}
	if outputs[0] == outputs[2] {
		t.Errorf("simkv: tape=60:9 did not change the faults")
	}
}

// TestSimTimeline checks that a failing run prints the fault timeline of
// simkv, its crashes and restarts, and that a passing run prints none of
// it.
func TestSimTimeline(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simkv")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	pass, _, err := runBin(bin, "GODETSEED=1")
	if err != nil {
		t.Fatal(err)
	}
	fail, _, err := runBin(bin, "GODETSEED=1", "SIMKV_FAIL=1")
	if err != nil {
		t.Fatal(err)
	}
	if pass.exitCode != 0 || fail.exitCode == 0 {
		t.Fatalf("simkv: exit status %d, and %d with SIMKV_FAIL=1; want 0 and a failure\n%s%s", pass.exitCode, fail.exitCode, fail.stdout, fail.stderr)
	}
	if strings.Contains(pass.stdout+pass.stderr, "sim: ") {
		t.Errorf("simkv passed and printed the timeline:\n%s%s", pass.stdout, pass.stderr)
	}
	for _, want := range []string{`(?m)^sim: 0\.000000000 world: new$`, `(?m)^sim: \d+\.\d{9} s1: power failure$`, `(?m)^sim: \d+\.\d{9} s2: process crash$`, `(?m)^sim: \d+\.\d{9} s1: stop$`, `(?m)^sim: \d+\.\d{9} s1: start$`} {
		if ok, _ := regexp.MatchString(want, fail.stderr); !ok {
			t.Errorf("simkv with SIMKV_FAIL=1: no line matching %s in\n%s", want, fail.stderr)
		}
	}
}

// TestSimLease checks that the simulated disk's slow syncs find the
// planted bug of testdata/simlease, a lease client that counts its term
// from after an fsync, and that the failure reproduces, while without the
// slow-disk faults no seed fails.
func TestSimLease(t *testing.T) {
	cfg := config{runs: 3, parallel: 4}
	x, err := exploreSeeds(cfg, "../../testdata/simlease", 1, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.failures) == 0 || !x.reproduced {
		t.Errorf("simlease: got %s, want a failure that reproduces", x.report())
	}
	t.Setenv("SIMLEASE_NODISK", "1")
	x, err = exploreSeeds(cfg, "../../testdata/simlease", 1, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.failures) > 0 {
		t.Errorf("simlease with SIMLEASE_NODISK=1: got %s, want no failures", x.report())
	}
}

// TestSimCompose checks that a workload under package detsched/sdk/compose
// (testdata/simcompose) runs deterministically and passes, and that its
// checks catch the seeded bug of its server, which acknowledges writes
// before syncing them.
func TestSimCompose(t *testing.T) {
	cfg := config{runs: 3, parallel: 3}
	// Which seeds catch the bug depends on the binary, so on the
	// architecture: look for one among the first 100. On linux/amd64 one
	// of the first 20 does; on darwin/arm64 about 1 in 15 does.
	caught := false
	for seed := 1; seed <= 100 && !caught; seed++ {
		for _, bug := range []string{"", "1"} {
			if bug == "" && seed > 2 {
				continue
			}
			t.Setenv("GODETSEED", strconv.Itoa(seed))
			t.Setenv("SIMCOMPOSE_BUG", bug)
			r, err := check(cfg, "../../testdata/simcompose")
			if err != nil {
				t.Fatal(err)
			}
			if !r.deterministic() {
				t.Fatalf("simcompose with GODETSEED=%d SIMCOMPOSE_BUG=%s is not deterministic:\n%s", seed, bug, r.details())
			}
			o := r.outcomes[0]
			if bug == "" && o.exitCode != 0 {
				t.Errorf("simcompose with GODETSEED=%d: exit status %d, stdout:\n%s", seed, o.exitCode, o.stdout)
			}
			caught = caught || bug != "" && o.exitCode != 0
		}
	}
	if !caught {
		t.Errorf("simcompose with SIMCOMPOSE_BUG=1 passed with seeds 1 to 100")
	}
}

// TestSimClients checks that a composer with several client nodes runs
// commands on each and that a partition can put two of them on different
// sides (testdata/simclients), deterministically.
func TestSimClients(t *testing.T) {
	for _, seed := range []string{"1", "2"} {
		t.Setenv("GODETSEED", seed)
		r, err := check(config{runs: 2, parallel: 2}, "../../testdata/simclients")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Fatalf("simclients with GODETSEED=%s is not deterministic:\n%s", seed, r.details())
		}
		if o := r.outcomes[0]; o.exitCode != 0 || !strings.Contains(o.stdout, "clients that ran commands: 3, two on different sides of a partition: true\n") {
			t.Errorf("simclients with GODETSEED=%s: exit status %d, stdout:\n%s", seed, o.exitCode, o.stdout)
		}
	}
}

// TestStops checks GODETSTOP on testdata/simcompose: the run stops at or
// after each point, at a tick point exactly when it falls in a
// goroutine's own code, ":dump" prints every goroutine, and the schedule
// stays as it was without stops.
func TestStops(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simcompose")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	stop := regexp.MustCompile(`(?m)^detsched: stop (\d) at t(-|\d+)(?:\.\d{9})? s(\d+) k(\d+) goroutine \d+$`)
	for _, seed := range []string{"1", "2", "3"} {
		env := []string{"GODETSEED=" + seed, "GODETBRANCH=0", "SIMCOMPOSE_BUG=1"}
		base, _, err := runBin(bin, env...)
		if err != nil {
			t.Fatal(err)
		}
		// Stop 2's tick is where stop 1 stopped.
		first, _, err := runBin(bin, append(env, "GODETSTOP=t5")...)
		if err != nil {
			t.Fatal(err)
		}
		m := stop.FindStringSubmatch(first.stderr)
		if m == nil {
			t.Fatalf("GODETSEED=%s GODETSTOP=t5: no stop:\n%s", seed, first.stderr)
		}
		k := m[4]
		points := []string{"t5", "k" + k, "s500:dump", "k1"}
		o, _, err := runBin(bin, append(env, "GODETSTOP="+strings.Join(points, ","))...)
		if err != nil {
			t.Fatal(err)
		}
		if o.trace != base.trace || o.exitCode != base.exitCode {
			t.Errorf("GODETSEED=%s: with stops, trace %q and exit status %d; without, %q and %d", seed, o.trace, o.exitCode, base.trace, base.exitCode)
		}
		fired := map[string]bool{}
		for _, m := range stop.FindAllStringSubmatch(o.stderr, -1) {
			fired[m[1]] = true
			switch m[1] {
			case "1":
				if n, _ := strconv.Atoi(m[2]); n < 5 {
					t.Errorf("GODETSEED=%s: stop t5 at %s", seed, m[0])
				}
			case "2":
				if m[4] != k {
					t.Errorf("GODETSEED=%s: stop k%s at %s", seed, k, m[0])
				}
			case "3":
				if n, _ := strconv.Atoi(m[3]); n < 500 {
					t.Errorf("GODETSEED=%s: stop s500 at %s", seed, m[0])
				}
			}
		}
		if len(fired) != len(points) {
			t.Errorf("GODETSEED=%s: stops %v fired, want all of %v:\n%s", seed, fired, points, o.stderr)
		}
		if n := strings.Count(o.stderr, "\ngoroutine "); n < 5 {
			t.Errorf("GODETSEED=%s: s500:dump printed %d goroutines:\n%s", seed, n, o.stderr)
		}
	}
}

// A branchOutput is the part of a composer run's standard error that a
// failing branch wrote, from its fault timeline on.
type branchOutput struct {
	text string
	late bool // the timeline had failed before it forked the branch
}

// branchOutputs returns the output of each failing branch in a composer
// run's standard error, by branch number. A branch is late when the first
// assertion that failed in the timeline's fault timeline is in the
// branch's too, which holds the timeline's events up to the fork.
func branchOutputs(stderr string) map[string]branchOutput {
	head := regexp.MustCompile(`(?m)^sim: fault timeline of (?:branch (\d+):|the timeline)`)
	at := regexp.MustCompile(`(?m)^sim: \d+\.\d{9} \S+: assertion failed: .*$`)
	hs := head.FindAllStringSubmatchIndex(stderr, -1)
	timeline := stderr // the timeline's part: before the first fault timeline, or after its own
	if len(hs) > 0 {
		timeline = stderr[:hs[0][0]]
	}
	branches := map[string]string{}
	for i, h := range hs {
		end := len(stderr)
		if i+1 < len(hs) {
			end = hs[i+1][0]
		}
		if h[2] < 0 {
			timeline = stderr[h[0]:end]
		} else {
			branches[stderr[h[2]:h[3]]] = stderr[h[0]:end]
		}
	}
	first := at.FindString(timeline)
	out := map[string]branchOutput{}
	for n, text := range branches {
		late := first != "" && slices.Contains(at.FindAllString(text, -1), first)
		out[n] = branchOutput{text, late}
	}
	return out
}

// TestSimBranchInline checks that GODETBRANCH=N runs the Nth terminal
// branch of a composer timeline in the process itself, as a debugger
// needs: the composer's output for the failing forked branch of
// testdata/simcompose names its GODETBRANCH setting, that setting
// reproduces the branch's failure with the branch's schedule, and passing
// over the branches of a passing run leaves its schedule as it was.
func TestSimBranchInline(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simcompose")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// Find a seed with a failing branch, which depends on the binary: on
	// darwin/arm64 about 1 in 30 seeds has one.
	var (
		env    []string
		forked outcome
		m      []string
		late   []string // a seed whose timeline failed before its branch did
	)
	for seed := 1; seed <= 200 && (m == nil || late == nil); seed++ {
		e := []string{fmt.Sprintf("GODETSEED=%d", seed), "SIMCOMPOSE_BUG=1"}
		o, _, err := runBin(bin, e...)
		if err != nil {
			t.Fatal(err)
		}
		bm := regexp.MustCompile(`compose: branch (\d+) runs the terminal phase; .* with (GODETBRANCH=(\d+)) added`).FindStringSubmatch(o.stdout)
		if bm == nil {
			continue
		}
		// The timeline writes its report before the output of a branch
		// it forked after it had failed, which only an anytime check
		// does, and after the output of the others.
		branches := branchOutputs(o.stderr)
		if b, ok := branches[bm[1]]; ok && !b.late && m == nil {
			env, forked, m = e, o, bm
		}
		for _, b := range branches {
			// (A late branch can fail with only the timeline's failure,
			// which it does not print again; skip those.)
			if late == nil && b.late && strings.Contains(b.text, "detsched: assertion failed: ") {
				late = e
				anytime := strings.Index(o.stderr, "detsched: assertion failed: compose: anytime")
				if anytime < 0 || strings.Index(o.stderr, b.text) < anytime {
					t.Errorf("%s: the timeline failed, but its branch's failure does not follow its report:\n%s", strings.Join(e, " "), o.stderr)
				}
			}
		}
	}
	if m == nil || m[1] != m[3] {
		t.Fatalf("no failing terminal branch names its GODETBRANCH setting in seeds 1 to 200:\n%s%s", forked.stdout, forked.stderr)
	}
	if late == nil {
		t.Errorf("no seed from 1 to 200 has a branch that failed after its timeline")
	}
	// The branch writes its output, then the timeline's.
	branch, _, _ := strings.Cut(forked.stdout, "\nFAIL\n")
	failure := regexp.MustCompile(`(?m)^detsched: assertion failed: compose: every terminal branch passes branch ` + m[1] + ` exited with status 1 \(` + m[2] + ` runs it in place\)`)
	if !failure.MatchString(forked.stdout + forked.stderr) {
		t.Errorf("the timeline's failure does not name branch %s and %s:\n%s%s", m[1], m[2], forked.stdout, forked.stderr)
	}
	// The branch's fault timeline says when its assertion failed.
	branchErr, _, _ := strings.Cut(forked.stderr, "sim: fault timeline of the timeline")
	if !regexp.MustCompile(`(?m)^sim: \d+\.\d{9} \S+: assertion failed: compose: `).MatchString(branchErr) {
		t.Errorf("the branch's fault timeline has no line for its failure:\n%s", branchErr)
	}
	inline, _, err := runBin(bin, append(env, m[2])...)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, _ := strings.Cut(inline.stdout, "\nFAIL\n"); got != branch || inline.exitCode == 0 {
		t.Errorf("%s: exit status %d, output\n%s\nwant the forked branch's\n%s", m[2], inline.exitCode, got, branch)
	}
	if inline.trace != forked.trace { // the first trace line, the branch's
		t.Errorf("%s: trace %q, want the forked branch's %q", m[2], inline.trace, forked.trace)
	}
	// A program the search runs to a stop, as it runs a cell's, forks no
	// branches, so a process forked from it writes the timeline's output
	// without theirs: here, a child that goes on as the timeline did,
	// from its last switch, writes what the run with GODETBRANCH=0 wrote.
	// The stopped program runs the schedule of GODETBRANCH=0, not the
	// forking run's timeline, which a branch's failure makes fail: on
	// failure, the test reports more at its end, which can take more
	// switches.
	timeline, _, err := runBin(bin, append(env, "GODETBRANCH=0")...)
	if err != nil {
		t.Fatal(err)
	}
	last := switchesField.FindStringSubmatch(timeline.trace + " ")
	if last == nil {
		t.Fatalf("GODETBRANCH=0: no trace line:\n%s", timeline.stderr)
	}
	srv, err := startForkServer(command(bin, env...))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()
	if ok, err := srv.run(atou(last[1]) - 1); !ok || err != nil {
		t.Fatalf("run to the last switch of GODETBRANCH=0's timeline, %s: %v, %v", last[1], ok, err)
	}
	child, err := srv.branchOutcome(1|detBranchGoesOn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if child.stdout != timeline.stdout || child.exitCode != timeline.exitCode {
		t.Errorf("forked from the program stopped after the branch: exit status %d, stdout\n%s\nwant %d and, as with GODETBRANCH=0,\n%s", child.exitCode, child.stdout, timeline.exitCode, timeline.stdout)
	}
	passing := []string{"GODETSEED=1"}
	a, _, err := runBin(bin, passing...)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := runBin(bin, append(passing, "GODETBRANCH=1000")...)
	if err != nil {
		t.Fatal(err)
	}
	if a.exitCode != 0 || a.trace != b.trace {
		t.Errorf("forking run: exit status %d, %s; GODETBRANCH=1000: %s", a.exitCode, a.trace, b.trace)
	}
}

// TestAssertAll checks that the runtime reports each failing assertion of
// testdata/assertall once, in order, with its first details and how many
// more times it failed.
func TestAssertAll(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/assertall")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	failed := regexp.MustCompile(`(?m)^detsched: assertion failed: .*$`)
	o, _, err := runBin(bin)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, w := range []string{"every item is valid map[item:0] (and 2 more failures)", "the end is unreachable map[items:3]"} {
		want = append(want, "detsched: assertion failed: "+w)
	}
	if got := failed.FindAllString(o.stderr, -1); !slices.Equal(got, want) || o.exitCode != 3 || o.stdout != "done\n" {
		t.Errorf("exit status %d, stdout %q, failures\n%s\nwant status 3, %q, and\n%s", o.exitCode, o.stdout, strings.Join(got, "\n"), "done\n", strings.Join(want, "\n"))
	}
}

// TestTestLogFork checks that a test that go test -v runs, which passes
// -test.testlogfile, a real file the testing package opens, still forks
// its composer branches. Before, every branch printed that it did not run
// because the process uses the real file system.
func TestTestLogFork(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simcompose")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := exec.Command(bin, "-test.v", "-test.testlogfile="+filepath.Join(t.TempDir(), "testlog"))
	cmd.Env = append(os.Environ(), "GODETSEED=1")
	out, err := cmd.CombinedOutput()
	if err != nil || bytes.Contains(out, []byte("cannot fork now")) {
		t.Errorf("%v\n%s", err, out)
	}
}

// TestForkTicks checks that a composer branch run in place
// (GODETBRANCH) reaches its first failed assertion at the tick its forked
// child did, so that GODETSTOP=kN on the replay line stops there. Before,
// the simulation's work in each forked child counted a tick.
func TestForkTicks(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simcompose")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	first := regexp.MustCompile(`first assertion failed at (\S+ s\d+ k\d+)`)
	branch := regexp.MustCompile(`detsched: branch (\d+) \(GODETBRANCH=`)
	for seed := 1; seed <= 40; seed++ {
		forked, _, err := runBin(bin, "SIMCOMPOSE_BUG=1", fmt.Sprintf("GODETSEED=%d", seed))
		if err != nil {
			t.Fatal(err)
		}
		m := branch.FindStringSubmatch(forked.stderr)
		if m == nil {
			continue
		}
		inPlace, _, err := runBin(bin, "SIMCOMPOSE_BUG=1", fmt.Sprintf("GODETSEED=%d", seed), "GODETBRANCH="+m[1])
		if err != nil {
			t.Fatal(err)
		}
		// The forked branch's output comes right after its line.
		_, after, _ := strings.Cut(forked.stderr, m[0])
		f, p := first.FindStringSubmatch(after), first.FindStringSubmatch(inPlace.stderr)
		if f == nil || p == nil || f[1] != p[1] {
			t.Errorf("seed %d, branch %s: forked %q, in place %q", seed, m[1], f, p)
		}
		return
	}
	t.Fatal("no seed failed in a branch")
}

// TestSimPrograms runs simulation programs that check themselves, with
// each seed, and checks that every run of a seed ends the same way, with
// the exit status and the messages, each printed once, that the case
// wants. The package comment of each program says what it guards.
func TestSimPrograms(t *testing.T) {
	cfg := config{runs: 2, parallel: 4}
	for _, tc := range []struct {
		pkg    string
		search string // GODETSEARCH
		seeds  []string
		exit   int
		want   []string // in the output, once each
	}{
		// Goroutines of no node cannot dial or listen while a World is
		// active, and the simulation says once why and what to do.
		{pkg: "simnonode", want: []string{"sim: a goroutine that belongs to no simulated node", "Node.Go"}},
		// Finalizers and cleanups run while a World always has a
		// goroutine to run. Under PCT the scheduler once picked a
		// runnable goroutine before it looked at the finalizer
		// goroutine, and with seed 3 none ran.
		{pkg: "simfinalizer", search: "perturb=pct", seeds: []string{"1", "3"}},
		// Every node has os.TempDir, here macOS's per-user directory.
		{pkg: "simtempdir"},
		// A watcher of file system changes on a node fails, and says why.
		{pkg: "simwatch", want: []string{"which creates a watcher of file system changes"}},
		// Finalizers and cleanups set in a bubble run in it, on their
		// node, and not at all once their node crashed. Before, they ran
		// outside every bubble, and closing a channel of the bubble ended
		// the program. With GODETSEED=7 the epoch comes from outside the
		// bubble while all its goroutines are blocked.
		{pkg: "simbubblefin", seeds: []string{"1", "2", "7"}},
		{pkg: "simbubblefin", search: "perturb=pct", seeds: []string{"1", "2"}},
		// One that blocks for good is in the bubble's deadlock report.
		{pkg: "simfinblock", exit: 2, want: []string{"main bubble goroutine has exited but blocked goroutines remain", "simfinblock.open.func1"}},
		// The CPU work of a simulation's nodes takes simulated time.
		{pkg: "simbusy"},
		// A node whose clock runs at another rate is consistent with
		// itself, across a change of rate and a restart, and a lease
		// that a server and its holder measure on their own clocks breaks
		// only when the holder's runs slow.
		{pkg: "simdrift", seeds: []string{"1", "2"}},
		// A run in which only the simulation's events run collects their
		// garbage. Before, a week of idle keepalives grew the heap by
		// 561 MB without a GC.
		{pkg: "simidlegc"},
		// A crash stops the node's tickers. Before, the ticker kept the
		// clock jumping from tick to tick: an hour of microsecond ticks
		// hit the switch limit.
		{pkg: "simdeadticker", search: "maxswitches=1000000"},
		// A panic in Config.OnPanic ends the program with both panics.
		// Before, its unwinding skipped to where the first panic had
		// stopped looking for defers: "fatal error: unknown pc".
		{pkg: "simpanicpanic", exit: 2, want: []string{"panic: on the node\n\tpanic: in OnPanic"}},
		// Node.Down is closed when a node ends, every way it ends.
		{pkg: "simdown", want: []string{"panic down", "kill down", "killprocess down", "stop down", "start returned"}},
		// A goroutine waiting for a mutex that a crashed node's sleeping
		// goroutine holds gets it once the holder wakes and releases it.
		{pkg: "simsharedlock", want: []string{"b took the lock"}},
		// A branch the process cannot fork keeps its number. Before, the
		// failing branch after it was named GODETBRANCH=1, which runs
		// the first.
		{pkg: "simbranchskip", exit: 3, want: []string{
			"detsched: branch 1 did not run: the process cannot fork now",
			"the second branch passes branch 2 exited with status 1 (GODETBRANCH=2 runs it in place)",
		}},
		// A forked branch that recovers from a nil dereference passes, as
		// it does in place; before, on macOS, it hung. A branch that
		// wait4 fails for is reported with the errno; before, as exit
		// status 1.
		{pkg: "simbranchsignal", exit: 3, want: []string{
			"the second branch passes branch 2: wait4 failed: ECHILD (GODETBRANCH=2 runs it in place)",
			"timeline done",
		}},
	} {
		if tc.seeds == nil {
			tc.seeds = []string{"1"}
		}
		t.Setenv("GODETSEARCH", tc.search)
		for _, seed := range tc.seeds {
			t.Setenv("GODETSEED", seed)
			r, err := check(cfg, "../../testdata/"+tc.pkg)
			if err != nil {
				t.Fatal(err)
			}
			if !r.deterministic() {
				t.Errorf("%s with GODETSEED=%s is not deterministic:\n%s", tc.pkg, seed, r.details())
				continue
			}
			o := r.outcomes[0]
			out := o.stdout + o.stderr
			ok := o.exitCode == tc.exit
			for _, w := range tc.want {
				ok = ok && strings.Count(out, w) == 1
			}
			if !ok {
				t.Errorf("%s with GODETSEED=%s: exit status %d, output:\n%s\nwant status %d and once each %q", tc.pkg, seed, o.exitCode, out, tc.exit, tc.want)
			}
		}
	}
}

// TestSimKill runs programs that kill a node in the middle of what its
// goroutines do, under many schedules, and checks that no schedule fails:
// the other nodes, and the node's next incarnation, must go on. The
// package comment of each program says what it guards. Before each fix,
// between a quarter and all of the seeds failed, except in simzombie's
// TestWaiter, where 6 of 80 hung, and in simkill's TestKillStarting under
// PCT, where seeds 2, 4 and 6 ran for hours.
func TestSimKill(t *testing.T) {
	cfg := config{parallel: 4}
	for _, tc := range []struct {
		pkg     string
		env     []string
		perturb string
		n       int
	}{
		{pkg: "simkill", perturb: "walk", n: 20},         // a goroutine runs after its memory is unmapped
		{pkg: "simkill", perturb: "pct", n: 20},          // tens of thousands of goroutines on a paused node
		{pkg: "simsharedunlock", perturb: "walk", n: 20}, // a mutex shared between nodes
		{pkg: "simstderr", n: 20},                        // os.Stderr's write lock
		{pkg: "simlookup", n: 20},                        // net.DefaultResolver's merged lookups
		{pkg: "simzombie", perturb: "walk", n: 40},       // a crashed node's mutexes
		{pkg: "simzombie", env: []string{"SIMZOMBIE_GC=1"}, perturb: "walk", n: 40},
		{pkg: "simzombie", env: []string{"SIMZOMBIE_RW=1"}, perturb: "walk", n: 40},
	} {
		t.Setenv("SIMZOMBIE_GC", "")
		t.Setenv("SIMZOMBIE_RW", "")
		for _, e := range tc.env {
			k, v, _ := strings.Cut(e, "=")
			t.Setenv(k, v)
		}
		x, err := exploreSeeds(cfg, "../../testdata/"+tc.pkg, 1, tc.n, tc.perturb)
		if err != nil {
			t.Fatal(err)
		}
		if len(x.failures) > 0 {
			t.Errorf("%s %s: got %s, want no failures", tc.pkg, strings.Join(tc.env, " "), x.report())
		}
	}
}

// TestManyRunnable checks that a switch costs little with tens of
// thousands of runnable goroutines, under each perturbation strategy
// (testdata/manyrunnable). The random walk's uniform pick walked the global
// run queue to its pick, and PCT scanned it for the highest priority, so
// each run took from half a minute to over ten.
func TestManyRunnable(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/manyrunnable")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, tc := range []struct{ seed, perturb string }{{"4", "walk"}, {"5", "walk"}, {"2", "pct"}} {
		cmd := command(bin, "GODETSEED="+tc.seed, "GODETSEARCH=perturb="+tc.perturb)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		timer := time.AfterFunc(30*time.Second, func() { cmd.Process.Kill() })
		err := cmd.Wait()
		timer.Stop()
		if wall := time.Since(start); err != nil || stdout.String() != "449985000\n" {
			t.Errorf("GODETSEED=%s perturb=%s: %v after %v, output %q, want 449985000 within 30s\n%s", tc.seed, tc.perturb, err, wall.Round(time.Millisecond), stdout.String(), stderr.String())
		}
	}
}

// TestPowerFailReports checks that a power failure reports torn writes
// as injected only on a node with unsynced writes, and lost entries of a
// strict disk only where a journaling disk would have kept them
// (testdata/simkill's TestPowerFailReports and TestStrictLossReports).
func TestPowerFailReports(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simkill")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cmd := command(bin, "GODETSEED=1", "GODETSEARCH=feedback=1")
	cmd.Args = append(cmd.Args, "-test.run=^(TestPowerFailReports|TestStrictLossReports)$")
	o, _, err := run(cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"torn", "strictdirsync"} {
		if m := regexp.MustCompile(`(?m)^detsched: assert reachable true=(\d+) false=\d+ fault: sim.disk.` + kind + `$`).FindStringSubmatch(o.stderr); o.exitCode != 0 || m == nil || m[1] != "1" {
			t.Errorf("exit status %d, sim.disk.%s reported %v times, want 1:\n%s", o.exitCode, kind, m, o.stderr)
		}
	}
}

// TestSimFaults checks that each fault of the simulation does what a
// program would see on a real system (testdata/simfaults), that a panic on
// a node crashes only that node with Config.OnPanic, and that the
// program runs deterministically.
func TestSimFaults(t *testing.T) {
	cfg := config{runs: 4, parallel: 4}
	for _, seed := range []string{"1", "2"} {
		t.Setenv("GODETSEED", seed)
		r, err := check(cfg, "../../testdata/simfaults")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Fatalf("simfaults with GODETSEED=%s is not deterministic:\n%s", seed, r.details())
		}
		o := r.outcomes[0]
		// What only Linux has: on darwin each mapping is a copy that
		// msync writes back, so shared mappings do not see each other's
		// stores at once; syscall.Syscall with a trap number, which the
		// program's msync uses, is not simulated, so stores through a
		// mapping are lost in a power failure; and there is no UDP GSO
		// or recvmmsg.
		linuxOnly := []string{
			"a second shared mapping sees a store at once:",
			"a private mapping's store stays its own:",
			"a store after a truncation:",
			"a store through a moved mapping:",
			`after a power failure: "synced" "" "" "written" "grown"`,
			"unsynced stores through two mappings:",
			"aliased mappings after a restart:",
			"aliased after a power failure:",
			"udp gso ",
			"udp recvmmsg:",
		}
		refused, reset := syscall.ECONNREFUSED.Error(), syscall.ECONNRESET.Error()
		for _, want := range []string{
			"process IDs of nodes and restarts are distinct, and not the test's: true",
			"baseline rtt a->b: 11ms ok",
			"slowed rtt a->b: 211ms ok",
			"jammed 35s, past the TCP timeout of 30s: rtt 35011ms ok",
			"stopped a->b: rtt 30010ms " + syscall.ETIMEDOUT.Error(), // "connection timed out" on Linux
			"rtt a->c during cut b->a: 11ms ok",
			"dial across a partition healed after 1s: ok after 1s",
			"dial with every packet a->c lost: " + syscall.ETIMEDOUT.Error() + " after 20s",
			"10 rtts with half the packets a->c lost take longer than 1s: true",
			"throttle(5) slows b's replies: true",
			"unthrottled again: true",
			"throttle ended at once: true",
			"skip back 1h: wall -1h0m0s, since 0s",
			"load a real time zone: ok",
			"rtt to paused b: deadline exceeded",
			"dial paused b: ok",
			"fsync with a failing disk: input/output error",
			`read after the failed fsync: "synced dropped"`,
			"fsync again: ok",
			"fsync waits for the device: true",
			"an O_SYNC write waits for the device: true",
			"sim.disk.slowsync stalls an fsync for 10ms or more: true, while the node's other goroutines run: true",
			"disk stalls: true, their fsyncs return when the stall ends: true, while a's other goroutines and b run: true",
			"fsyncs on a slowed disk take 5 times as long or more: true",
			"an excluded node: node faults hit it: false, hit servers: true, partitioned with some servers and not others: true, an end of a faulty link: true, clogged: true",
			`after a power failure: "synced "`,
			"fsync of a redirtied page: ok",
			"fsync of an exempt file with a failing disk: ok",
			`read of a big mapping's first page: "head"`,
			`redirtied page after a power failure: "synced dropped redirtied"`,
			"read with a sector going bad in a calm: ok",
			"read again: input/output error",
			"read after rewriting: ok",
			"create on a full disk: no space left on device",
			"append on a full disk: no space left on device",
			"append after freeing space: ok",
			"torn write: sectors are whole or lost: true",
			`hard link: ok, again: file exists, after removing the original: "linked"`,
			`O_SYNC write after a power failure: "durable"`,
			`hard link after a power failure: "linked"`,
			`mmap: ok, reads "mapped"`,
			`read sees a write to a shared mapping: "written"`,
			`mremap: the mapping keeps "written"`,
			`unmapped by a goroutine of no node: ok, "unmapped", then a write: ok`,
			`after a process crash: "synced" "kept" "written" "grown"`,
			`after a power failure: "synced" "" "" "written" "grown"`,
			`the branch's store stays in the branch: ""`,
			`a second shared mapping sees a store at once: "aliased"`,
			`a private mapping's store stays its own: "aliased" "aliased"`,
			`read sees a store through a mapping: "aliased"`,
			`both mappings see a write: "aliawritten" "aliawritten"`,
			`a truncation clears the mapping: "", and growing again: ""`,
			`a store after a truncation: "after"`,
			`a store through a moved mapping: "moved"`,
			`a private mapping after the last one went: "again" "stored"`,
			"msync of an aliased mapping: ok",
			`unsynced stores through two mappings: "one" "two"`,
			`aliased after a process crash: "synced" "one" "two"`,
			`aliased mappings after a restart: "three" "four"`,
			`aliased after a power failure: "synced" "" "" "" ""`,
			"OnPanic: a: assignment to entry in nil map",
			"after a panic on a: a up false, b up true",
			"OnPanic: a: fatal error: sync: unlock of unlocked mutex",
			"after a fatal error on a: a up false, b up true",
			"OnPanic: a: fatal error: sync: unlock of unlocked mutex\n\twhile panicking: boom",
			"the node ran during OnPanic: false",
			"OnPanic: a: os.Exit(3)",
			"after os.Exit on a: a up false, b up true",
			"OnPanic: a: fatal error: unexpected fault address, in readUnmapped: true",
			"after a fault on a: a up false, b up true",
			"bind to the port of a refused connect: ok",
			"dial twice from port 5000 to a closed port: " + refused + ", " + refused,
			"the first read of a 64KiB write gets part of it: true ok",
			"write 8MiB across a stopped link: 4194304 bytes, deadline exceeded",
			"write 16MiB to a reader that does not read: 10485760 bytes, deadline exceeded",
			"once it reads: 16777216 bytes, ok",
			`the timed-out end reads what had arrived: "hello" ok`,
			"then its error: ETIMEDOUT true",
			"then: EOF",
			"the other end has not failed: deadline exceeded",
			"the other end writes, is reset, and writes twice more: ok, " + reset + ", " + syscall.EPIPE.Error(),
			"idle read from b after a power failure (restarted false): " + syscall.ETIMEDOUT.Error() + " after 2m29s",
			"idle read from b after a power failure (restarted true): " + reset + " after 14s",
			"stacks of b: as many as Goroutines true, shows b's true, shows a's false",
			"dial b after its process crashed: " + refused + " after 20ms",
			"dial b after a power failure: deadline exceeded",
			"the server's writes to a client that lost power mid-handshake: " + reset,
			"the bubble goes on after the world closed: ok",
			"udp a->b: 2 bytes from 10.0.0.1:4000 after 10ms ok",
			"udp ecn bits arrive: 2 ok",
			"udp truncated read: 100, MSG_TRUNC true ok",
			"udp send over the interface MTU: " + syscall.EMSGSIZE.Error(),
			"udp gso 2500 bytes in segments of 1000: [500 1000 1000]",
			"udp datagram over the path MTU: deadline exceeded",
			"udp across a stopped link: deadline exceeded",
			"udp in flight when the link stops: deadline exceeded",
			"udp from behind a NAT: from a public port true ok",
			"udp a reply to the public port: ok",
			"udp after a rebinding: from a new port true ok",
			"udp a reply to the old public port: deadline exceeded",
			"udp across a jammed link healed after 1s: after 1.01s ok",
			"udp a full buffer drops datagrams: true, marks some CE: true",
			"udp recvmmsg: 3 datagrams, first 1 bytes, errno 0",
			"udp to a node whose process crashed: ok",
		} {
			if runtime.GOOS != "linux" && slices.ContainsFunc(linuxOnly, func(p string) bool { return strings.HasPrefix(want, p) }) {
				continue
			}
			if !strings.Contains(o.stdout, want+"\n") && !strings.Contains(o.stdout, want+" ") {
				t.Errorf("simfaults with GODETSEED=%s: missing %q in output:\n%s", seed, want, o.stdout)
			}
		}
		if strings.Contains(o.stdout, "the branch reads") {
			t.Errorf("simfaults with GODETSEED=%s: a branch's write to a mapped page did not reach the file:\n%s", seed, o.stdout)
		}
		// The second world reuses no descriptor numbers of the first,
		// whose crashed nodes left sockets open.
		if !regexp.MustCompile(`\nrestarted a reaches b: \d+ms ok\n`).MatchString(o.stdout) {
			t.Errorf("simfaults with GODETSEED=%s: the restarted node did not reach b:\n%s", seed, o.stdout)
		}
	}
}

// TestSimNemesisLock checks that a nemesis callback that waits for a node
// the nemesis paused fails the program with a clear message, the same in
// every run (testdata/simnemesislock): at once when it is about to wait
// for a mutex that a goroutine of the node holds, and after a minute of
// the bubble's time when it waits on a channel. Before, the callback
// waited forever while another node's ticker moved the clock on, and the
// program never ended. A normal nemesis run, whose callbacks wait for a
// node that the test, not the nemesis, paused, passes.
func TestSimNemesisLock(t *testing.T) {
	cfg := config{runs: 3, parallel: 3}
	t.Setenv("GODETSEED", "1")
	for _, tc := range []struct {
		mode string
		want string // a regexp
	}{
		{"", `detsim: the nemesis's Event callback \(goroutine \d+\) waits for the sync\.Mutex at 0x[0-9a-f]+ that goroutine \d+ of node a holds, and the nemesis paused a: it would wait forever, as only the nemesis resumes the node\n` +
			`\ngoroutine \d+ \[sleep[^\n]*\n(?s:.*)` +
			`fatal error: detsim: a nemesis callback waits for a lock that a node the nemesis paused holds; callbacks must not take locks that node goroutines hold\n` +
			`(?s:.*)\ngoroutine \d+ \[running[^\n]*\n(?s:.*)sim\.\(\*Nemesis\)\.event`},
		{"chan", `detsim: the nemesis's Event callback \(goroutine \d+\) has been blocked for 60s of the bubble's time\n` +
			`detsim: nodes the nemesis paused: a\n` +
			`\ngoroutine \d+ \[chan receive[^\n]*\n(?s:.*)sim\.\(\*Nemesis\)\.event(?s:.*)` +
			`fatal error: detsim: a nemesis callback has been blocked for too long; callbacks must return promptly, and must not wait for nodes the nemesis paused\n`},
	} {
		t.Setenv("SIMNEMESISLOCK", tc.mode)
		r, err := check(cfg, "../../testdata/simnemesislock")
		if err != nil {
			t.Fatal(err)
		}
		if !r.deterministic() {
			t.Errorf("simnemesislock with SIMNEMESISLOCK=%s is not deterministic:\n%s", tc.mode, r.details())
			continue
		}
		o := r.outcomes[0]
		if o.exitCode != 2 || !regexp.MustCompile(tc.want).MatchString(o.stderr) {
			t.Errorf("simnemesislock with SIMNEMESISLOCK=%s: exit status %d, stderr:\n%s", tc.mode, o.exitCode, o.stderr)
		}
	}

	t.Setenv("SIMNEMESISLOCK", "normal")
	r, err := check(cfg, "../../testdata/simnemesislock")
	if err != nil {
		t.Fatal(err)
	}
	if !r.deterministic() || r.outcomes[0].exitCode != 0 || !strings.HasPrefix(r.outcomes[0].stdout, "events: ") {
		t.Fatalf("simnemesislock with SIMNEMESISLOCK=normal: got %s, want a deterministic pass:\n%s", r.summary(), r.details())
	}
}

// TestStrategySwarm checks that by default each seed picks the random walk
// or PCT, and that the trace report names the strategy.
func TestStrategySwarm(t *testing.T) {
	cfg := config{parallel: 4}
	bin, cleanup, err := build(cfg, "../../testdata/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	outs, _, err := runAll(cfg, bin, 20, func(i int) []string {
		return []string{fmt.Sprintf("GODETSEED=%d", i)}
	})
	if err != nil {
		t.Fatal(err)
	}
	strategies := map[string]int{}
	for i, o := range outs {
		m := perturbLine.FindStringSubmatch(o.stderr)
		if m == nil {
			t.Fatalf("seed %d printed no perturbation line:\n%s", i, o.stderr)
		}
		strategies[strings.Fields(m[1])[0]]++
	}
	if len(strategies) != 2 || strategies["walk"] == 0 || strategies["pct"] == 0 {
		t.Errorf("strategies picked by 20 seeds: %v, want both walk and pct", strategies)
	}
}

// TestEnvironment checks that unrelated environment variables, which
// change the environment's size and the number of variables syscall
// scans, do not change the schedule of a program that collects garbage
// and looks up a variable.
func TestEnvironment(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/gcstress")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	var outs []outcome
	for _, extra := range [][]string{nil, {"DETCHECK_A=1"}, {"DETCHECK_B=" + strings.Repeat("x", 10000)}} {
		o, _, err := runBin(bin, append([]string{"GODETSEED=7", "GODETSEARCH=perturb=walk"}, extra...)...)
		if err != nil {
			t.Fatal(err)
		}
		outs = append(outs, o)
	}
	if outs[0] != outs[1] || outs[0] != outs[2] {
		t.Errorf("gcstress: outcomes differ with extra environment variables:\n%+v\n%+v\n%+v", outs[0], outs[1], outs[2])
	}
}

// TestNonblockingStdio checks that whether the standard descriptors a
// program inherits are nonblocking, as a pipe from some tools is and a
// debugger's terminal is not, does not change the schedule.
func TestNonblockingStdio(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for seed := 1; seed <= 5; seed++ {
		env := "GODETSEED=" + strconv.Itoa(seed)
		want, _, err := runBin(bin, env)
		if err != nil {
			t.Fatal(err)
		}
		var in, out [2]int
		if err := syscall.Pipe(in[:]); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Pipe(out[:]); err != nil {
			t.Fatal(err)
		}
		syscall.SetNonblock(in[0], true)
		syscall.SetNonblock(out[1], true)
		inR, inW := os.NewFile(uintptr(in[0]), "stdin"), os.NewFile(uintptr(in[1]), "stdin writer")
		outR, outW := os.NewFile(uintptr(out[0]), "stdout reader"), os.NewFile(uintptr(out[1]), "stdout")
		cmd := command(bin, env)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
		err = cmd.Start()
		inR.Close()
		outW.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(outR)
		outR.Close()
		cmd.Wait()
		inW.Close()
		if got := newOutcome("", string(b), cmd.ProcessState.ExitCode()); got.trace != want.trace {
			t.Errorf("%s: nonblocking standard descriptors: %s, blocking: %s", env, got.trace, want.trace)
		}
	}
}

// TestTempNames checks that a run whose temporary names are taken, as
// they are while another run of its seed has its files, takes others of
// the same length and keeps its schedule.
func TestTempNames(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/tempname")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := []string{"GODETSEED=3", "TEMPNAME_DIR=" + t.TempDir()}
	first, _, err := runBin(bin, env...)
	if err != nil || first.exitCode != 0 {
		t.Fatalf("first run: %v, %+v", err, first)
	}
	second, _, err := runBin(bin, env...)
	if err != nil || second.exitCode != 0 {
		t.Fatalf("second run: %v, %+v", err, second)
	}
	a, b := strings.Fields(first.stdout), strings.Fields(second.stdout)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("output %q and %q, want two names each", first.stdout, second.stdout)
	}
	for i := range a {
		if a[i] == b[i] || len(a[i]) != len(b[i]) {
			t.Errorf("names %q then %q, want different names of the same length", a[i], b[i])
		}
	}
	if first.trace != second.trace {
		t.Errorf("with its names taken: %s, first: %s", second.trace, first.trace)
	}
}

// TestNoASLREnvironment checks that a program that runs itself again
// without ASLR, as one does on macOS, sees the environment it was started
// with, as one started without ASLR does: not the variables the runtime
// adds for the second run, unless they were set already.
func TestNoASLREnvironment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS runs a program again without ASLR")
	}
	bin, cleanup, err := build(config{}, "../../testdata/environ")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	const objc = "OBJC_DISABLE_INITIALIZE_FORK_SAFETY"
	for _, set := range []bool{false, true} {
		cmd := command(bin, "GODETSEED=1")
		cmd.Env = slices.DeleteFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, objc+"=") })
		if set {
			cmd.Env = append(cmd.Env, objc+"=YES")
		}
		o, _, err := run(cmd)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(o.stdout, objc+"="); got != set || strings.Contains(o.stdout, "GODETNOASLR") {
			t.Errorf("%s set: %t; the program sees:\n%s", objc, set, o.stdout)
		}
	}
}

// TestDetGodebug checks that the runtime's own GODEBUG settings, which
// package internal/godebug would otherwise parse and allocate for, do not
// change the schedule, so that diagnostic settings can be added to replay
// a seed (testdata/godebugkeys). dettrace=2 also looks up the function of
// every switch, which draws from the M's random state; the caches of type
// assertions and type switches must not, or the run allocates another
// one (testdata/typecache).
func TestDetGodebug(t *testing.T) {
	for _, prog := range []string{"godebugkeys", "typecache"} {
		bin, cleanup, err := build(config{}, "../../testdata/"+prog)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		var outs []outcome
		for _, godebug := range []string{"", "dettrace=2", "dettrace=1,detxxxxx=1"} {
			o, _, err := runBin(bin, "GODETSEED=1", "GODEBUG="+godebug)
			if err != nil {
				t.Fatal(err)
			}
			if o.exitCode != 0 {
				t.Fatalf("%s with GODEBUG=dettrace=1,%s: exit status %d\n%s%s", prog, godebug, o.exitCode, o.stdout, o.stderr)
			}
			outs = append(outs, o)
		}
		for i, godebug := range []string{"dettrace=2 added", "dettrace=1,detxxxxx=1 added"} {
			if o := outs[i+1]; o.trace != outs[0].trace || o.stdout != outs[0].stdout {
				t.Errorf("%s with %s: got %s, %q; want %s, %q", prog, godebug, o.trace, o.stdout, outs[0].trace, outs[0].stdout)
			}
		}
	}
}

// TestDebugBuild checks that the build without optimizations, which debug
// lines run, runs the optimized build's schedule and prints the same: in a
// program that reads its build info, which records -gcflags only in the
// build without optimizations (testdata/buildinfo), and in one that makes
// types with reflect.ArrayOf, which searches the binary's types, and looks
// them up in a sync.Map, whose work depends on their addresses; both
// differ between the builds (testdata/typemap); and in one that ranges
// over maps keyed by pointers, whose addresses differ between the builds
// (testdata/ptrmap); and in compute-bound goroutines that preempt each
// other all over constructs the builds compile differently, such as a
// switch on strings of eight lengths, which only the optimized build
// compiles to a jump table (testdata/constructs).
func TestDebugBuild(t *testing.T) {
	for _, prog := range []string{"buildinfo", "typemap", "ptrmap", "constructs"} {
		var outs []outcome
		for _, flags := range [][]string{nil, {"-gcflags=all=-N -l"}} {
			_, bin, err := buildPackage("../../testdata/"+prog, t.TempDir(), flags...)
			if err != nil {
				t.Fatal(err)
			}
			o, _, err := runBin(bin, "GODETSEED=1")
			if err != nil {
				t.Fatal(err)
			}
			if o.exitCode != 0 {
				t.Fatalf("%s built with %q: exit status %d\n%s%s", prog, flags, o.exitCode, o.stdout, o.stderr)
			}
			outs = append(outs, o)
		}
		if outs[1].trace != outs[0].trace || outs[1].stdout != outs[0].stdout {
			t.Errorf("%s built with -gcflags=all=-N -l: %s, %q; optimized: %s, %q", prog, outs[1].trace, outs[1].stdout, outs[0].trace, outs[0].stdout)
		}
	}
}

// TestRangeGC checks that a range loop over a slice whose elements the
// compiler reaches through a uintptr keeps the slice live where detsched
// preempts the loop, so that a GC meanwhile does not free it
// (testdata/rangegc).
func TestRangeGC(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/rangegc")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, q := range []string{"3", "5", "7"} {
		o, _, err := runBin(bin, "GODETSEED=1", "GODEBUG=clobberfree=1", "GODETSEARCH=perturb=off quantum="+q)
		if err != nil {
			t.Fatal(err)
		}
		if o.exitCode != 0 {
			t.Errorf("quantum=%s: %s%s", q, o.stdout, o.stderr)
		}
	}
}

// TestOutput checks that output written to fds 1 and 2, which det mode
// keeps in memory, is written at exit, in order, also when the program
// panics or exits with a nonzero status.
func TestOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "output")
	cmd := exec.Command(goCmd(), "build", "-o", bin, "../../testdata/output")
	cmd.Env = append(os.Environ(), "GOEXPERIMENT=detsched")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	// 10000 lines span several 64 KiB buffer chunks.
	var full strings.Builder
	for i := range 10000 {
		fmt.Fprintf(&full, "line %d\n", i)
		if i%1000 == 999 {
			fmt.Fprintf(&full, "stderr after line %d\n", i)
		}
	}
	const panicMsg = "panic: boom\n"

	for _, tt := range []struct {
		exit     string
		want     string // prefix of the combined output
		wantCode int
	}{
		{"", full.String(), 0},
		{"3", full.String(), 3},
		{"panic", full.String() + panicMsg, 2},
	} {
		cmd := exec.Command(bin)
		cmd.Env = append(os.Environ(), "OUTPUT_LINES=10000", "OUTPUT_EXIT="+tt.exit)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out // one pipe, to check the order across fds
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		got := out.String()
		if code != tt.wantCode || !strings.HasPrefix(got, tt.want) || tt.exit != "panic" && got != tt.want {
			if len(got) > 200 {
				got = got[:100] + " ... " + got[len(got)-100:]
			}
			t.Errorf("exit %q: got status %d, output %q; want status %d and %d bytes of output",
				tt.exit, code, got, tt.wantCode, len(tt.want))
		}
	}
}
