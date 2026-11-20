// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestFork checks that a child forked at switch N with seed S (and PCT
// horizon K) produces exactly what re-executing the program with
// the tape N:S (or N:S:K) produces, with and without perturbation.
// testdata/synctest is not among the programs: it sleeps outside its bubble
// in real time, so even re-executions of a perturbed run of it differ.
func TestFork(t *testing.T) {
	cfg := config{}
	for _, pkg := range []string{"gcstress", "nonidle", "spin", "selectorder", "simkv"} {
		bin, cleanup, err := build(cfg, "../../testdata/"+pkg)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		for _, godebug := range []string{"off", "walk", "pct"} {
			env := []string{"GODETSEED=4", "GODETSEARCH=perturb=" + godebug}
			// A run leaves the deterministic subset when a system call
			// takes 10ms of real time, such as getcwd in package os's
			// init on a loaded macOS machine, and then differs.
			for try := 1; compareForks(t, pkg+" "+godebug, bin, env); try++ {
				if try == 3 {
					t.Errorf("%s %s: a run left the deterministic subset %d times", pkg, godebug, try)
					break
				}
				t.Logf("%s %s: a run left the deterministic subset; trying again", pkg, godebug)
			}
		}
	}
}

// compareForks runs bin with env under a fork server and compares children
// forked at a few switches with re-executions, as TestFork describes. It
// reports whether it stopped because a child or a re-execution left the
// deterministic subset, which makes the runs differ by design.
func compareForks(t *testing.T, name, bin string, env []string) (left bool) {
	t.Helper()
	s, err := startForkServer(command(bin, env...))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	defer s.close()
	compared := 0
	// At switch 2 the runtime's init runs with the P on another thread,
	// so that stop waits for it to come back.
	for _, n := range []uint64{1, 2, 7, 30} {
		// The server stops first at switch 1, and a run to a switch at
		// or before the current one runs to the end.
		if n != s.at {
			ok, err := s.run(n)
			if err != nil {
				t.Fatalf("%s: run to %d: %v", name, n, err)
			}
			if !ok {
				break
			}
		}
		// Seed 2 also sets PCT's horizon.
		for _, seed := range []uint64{1, 2} {
			steps := (seed - 1) * 5000
			got, err := s.branchOutcome(seed, steps)
			if _, ok := err.(errRefused); ok {
				if n == 2 {
					t.Errorf("%s: branch at %d (stopped at 2): %v", name, s.at, err)
				}
				// Such as with an M in a system call.
				t.Logf("%s: branch at %d: %v", name, n, err)
				break
			}
			if err != nil {
				t.Fatalf("%s: branch at %d: %v", name, n, err)
			}
			// A stop waits for the P to be on the main thread, so it may
			// be after n.
			tape := fmt.Sprintf("GODETSEARCH=tape=%d:%d", s.at, seed)
			if steps > 0 {
				tape += fmt.Sprintf(":%d", steps)
			}
			want, _, err := runBin(bin, append(env, tape)...)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				// A child also reports what the server did before the fork.
				const leftLine = "left the deterministic subset"
				if strings.Contains(got.stderr, leftLine) || strings.Contains(want.stderr, leftLine) {
					return true
				}
				t.Errorf("%s %s: forked child differs from re-execution:\nforked: %+v\nre-run: %+v", name, tape, got, want)
			}
			compared++
		}
	}
	if compared == 0 {
		t.Errorf("%s: no branch was compared", name)
	}
	return false
}

// TestForkIdleM checks that a child forked while an M is parked idle
// starts it on a new thread when it needs it.
func TestForkIdleM(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/forkidle")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	s, err := startForkServer(command(bin))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if ok, err := s.run(60); !ok || err != nil {
		t.Fatalf("run to switch 60: %v, %v", ok, err)
	}
	type result struct {
		o   outcome
		err error
	}
	c := make(chan result, 1)
	go func() {
		o, err := s.branchOutcome(1, 0)
		c <- result{o, err}
	}()
	select {
	case r := <-c:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.o.exitCode != 0 || r.o.stdout != "phase 1 handed off: true\nphase 2 handed off: true\n" {
			t.Errorf("forked child: exit status %d, stdout %q; want 0 and two hand-offs", r.o.exitCode, r.o.stdout)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("forked child did not finish in 20s")
	}
}

// TestForkTree checks branches of branches: a child forked with its own
// control socket runs on and forks a grandchild, which must match
// re-execution with the two-entry tape.
func TestForkTree(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/gcstress")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := []string{"GODETSEED=4", "GODETSEARCH=perturb=walk"}
	s, err := startForkServer(command(bin, env...))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if ok, err := s.run(7); !ok || err != nil {
		t.Fatalf("run to switch 7: %v, %v", ok, err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	pid, child, err := s.branch(1, 0, branchFiles{stdout: devnull, stderr: devnull, control: true})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := child.run(30); !ok || err != nil {
		t.Fatalf("child: run to switch 30: %v, %v", ok, err)
	}
	got, err := child.branchOutcome(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := runBin(bin, append(env, "GODETSEARCH=tape=7:1,30:2")...)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("grandchild differs from re-execution:\nforked: %+v\nre-run: %+v", got, want)
	}
	if ok, err := child.run(0); ok || err != nil {
		t.Fatalf("child: run to end: %v, %v", ok, err)
	}
	child.close()
	if code, err := s.wait(pid); code != 0 || err != nil {
		t.Errorf("child: exit status %d, %v", code, err)
	}
}

// TestForkStoppedBranches checks that a program the search runs to a stop
// forks none of the composer branches it passes (testdata/simbranchstop,
// whose branches all fail and run on past the next stop): before, a
// branch a setup root or a cell's program forked failed in every run
// forked from it, and its replay line applied the run's branch points
// inside the branch, which ran another schedule. A run forked from a
// cell's program reports a branch it reached itself, and the replay
// lines of the branch and of the timeline, with the run's tape, run the
// schedules the run printed.
func TestForkStoppedBranches(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/simbranchstop")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := []string{"GODETSEED=1"}
	base, _, err := runBin(bin, append(env, "GODETBRANCH=0")...)
	if err != nil {
		t.Fatal(err)
	}
	switches := atou(switchesField.FindStringSubmatch(base.trace + " ")[1])
	s, err := startForkServer(command(bin, env...))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	// A cell's program, as a search stops one: the root runs to a third
	// of the run and forks a child that goes on as it, to two thirds.
	if ok, err := s.run(switches / 3); !ok || err != nil {
		t.Fatalf("run to switch %d: %v, %v", switches/3, ok, err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	pid, cell, err := s.branch(1|detBranchGoesOn, 0, branchFiles{stdout: devnull, stderr: devnull, control: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cell.close()
		s.wait(pid)
	}()
	if ok, err := cell.run(2 * switches / 3); !ok || err != nil {
		t.Fatalf("cell: run to switch %d: %v, %v", 2*switches/3, ok, err)
	}
	got, err := cell.branchOutcome(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`branch (\d+) fails`).FindStringSubmatch(got.stdout)
	traces := traceLine.FindAllString(got.stderr, -1)
	if got.exitCode == 0 || m == nil || len(traces) == 0 {
		t.Fatalf("forked run: exit status %d, want a failing branch, and the timeline's trace line after the branch's:\n%+v", got.exitCode, got)
	}
	tape := fmt.Sprintf("GODETSEARCH=tape=%d:%d,%d:2", s.at, uint64(1|detBranchGoesOn), cell.at)
	branch, _, err := runBin(bin, append(env, tape, "GODETBRANCH="+m[1])...)
	if err != nil {
		t.Fatal(err)
	}
	if branch.trace != got.trace || !strings.Contains(branch.stdout, m[0]) {
		t.Errorf("GODETBRANCH=%s %s ran %s, printing %q; the forked run's branch ran %s", m[1], tape, branch.trace, branch.stdout, got.trace)
	}
	timeline, _, err := runBin(bin, append(env, tape, "GODETBRANCH=0")...)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimPrefix(traces[0], "detsched: "); timeline.trace != want {
		t.Errorf("GODETBRANCH=0 %s ran %s; the forked run's timeline ran %s", tape, timeline.trace, want)
	}
}

// TestForkRefused checks that a program with a file registered with the
// network poller, or a regular file open, refuses to fork, and that it
// continues normally after.
func TestForkRefused(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/forkpoll")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	s, err := startForkServer(command(bin))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var forked int
	refused := map[uint64]int{}
	for n := uint64(10); ; n += 10 {
		ok, err := s.run(n)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		o, err := s.branchOutcome(1, 0)
		var r errRefused
		switch {
		case errors.As(err, &r) && (r.reason == 4 || r.reason == forkFiles):
			refused[r.reason]++
		case err != nil:
			t.Fatalf("branch at %d: %v", n, err)
		case o.exitCode != 0 || !strings.HasSuffix(o.stdout, "done\n"):
			t.Errorf("branch at %d: %+v", n, o)
		default:
			forked++
		}
	}
	if forked == 0 || refused[4] == 0 || refused[forkFiles] == 0 {
		t.Errorf("forked %d times, refused %d times for a pipe and %d times for a file; want all three", forked, refused[4], refused[forkFiles])
	}
}

// TestForkMany forks many children from one stop and checks a sample of
// them against re-execution, and that the server leaks no descriptors.
func TestForkMany(t *testing.T) {
	bin, cleanup, err := build(config{}, "../../testdata/gcstress")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := []string{"GODETSEED=4"}
	s, err := startForkServer(command(bin, env...))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if ok, err := s.run(30); !ok || err != nil {
		t.Fatalf("run to switch 30: %v, %v", ok, err)
	}
	fds := func() int {
		if runtime.GOOS == "darwin" {
			out, _ := exec.Command("lsof", "-a", "-p", fmt.Sprint(s.cmd.Process.Pid), "-d", "0-65535", "-F", "f").Output()
			return strings.Count(string(out), "\nf")
		}
		ents, _ := os.ReadDir(fmt.Sprintf("/proc/%d/fd", s.cmd.Process.Pid))
		return len(ents)
	}
	before := fds()
	const n = 32
	for seed := uint64(1); seed <= n; seed++ {
		got, err := s.branchOutcome(seed, 0)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if seed%8 != 0 {
			continue
		}
		want, _, err := runBin(bin, append(env, fmt.Sprintf("GODETSEARCH=tape=30:%d", seed))...)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("seed %d: forked child differs from re-execution:\nforked: %+v\nre-run: %+v", seed, got, want)
		}
	}
	if after := fds(); after != before {
		t.Errorf("server has %d descriptors after %d forks, %d before", after, n, before)
	}
}

// TestSearchGuided checks that the search keeps runs that reached
// something new and finds stagedbug's failure, which needs three rare
// events in a row, and that the printed command replays it, tape and all.
func TestSearchGuided(t *testing.T) {
	t.Setenv("SETUP", "1000")
	s := runSearch(t, "../../testdata/stagedbug", 3000)
	g := findGroup(s, "all three stages lost their race")
	if s.arms[armCell].launches == 0 || g == nil || g.status != "reproduced" || !strings.Contains(g.replay, "tape=") {
		t.Errorf("want runs from cells and stagedbug's failure, replayed with its tape:\n%s", searchReport(s))
	}
}

// TestSearchDirected checks that the search directs runs at the deadlock
// that bugrarelock's lock order predicts, which undirected runs find in
// about 1 in 70, and that the printed command replays it.
func TestSearchDirected(t *testing.T) {
	s := runSearch(t, "../../testdata/bugrarelock", 1000)
	g := findGroup(s, "deadlock")
	if len(s.predicted) == 0 || s.arms[armDirected].launches == 0 || g == nil || g.status != "reproduced" {
		t.Errorf("want a predicted deadlock, directed runs, and a deadlock replayed:\n%s", searchReport(s))
	}
}

// TestSearchDirectedHolds checks that runs directed at a predicted
// deadlock that cannot happen pass when goroutines lock their own mutexes
// at the predicted site: holding each there must not make the run look
// like a livelock, as about 1 in 25 runs did when holders yielded to each
// other.
func TestSearchDirectedHolds(t *testing.T) {
	s := runSearch(t, "../../testdata/lockholds", 300)
	if len(s.predicted) == 0 || s.directed[0] == 0 || s.failed != 0 {
		t.Errorf("want a predicted deadlock, directed runs, and no failure:\n%s", searchReport(s))
	}
}

// TestSearchSetup checks that the search starts its runs right after the
// program's setup, that an assertion failure counts as a failure that its
// command replays, and that the summary says how many runs evaluated the
// always assertion.
func TestSearchSetup(t *testing.T) {
	s := runSearch(t, "../../testdata/sdk", 200)
	g := findGroup(s, "detsched: assertion failed: no increment is lost")
	if s.stop < 2 || g == nil || g.status != "reproduced" || g.runs == s.runs {
		t.Errorf("want some but not all runs failing the assertion, replayed:\n%s", searchReport(s))
	}
	if r := searchReport(s); !regexp.MustCompile(`\ndetsched: runs that evaluated each always assertion .*: "no increment is lost" [1-9]\d*\n`).MatchString(r) {
		t.Errorf("summary does not count the runs that evaluated the always assertion:\n%s", r)
	}
}

// TestSearchKinds checks that a search makes a group of the runs that fail
// each assertion, whatever their details, even an error among others, but
// for each error, numbers aside, of one whose details are only an error,
// and that a group lists
// another assertion its runs fail with the log of the first run that
// failed it.
func TestSearchKinds(t *testing.T) {
	s := runSearch(t, "../../testdata/assertkinds", 300)
	first := findGroup(s, "detsched: assertion failed: the first invariant holds")
	second := findGroup(s, "detsched: assertion failed: the second invariant holds")
	leaders := findGroup(s, `detsched: assertion failed: the check succeeds map[err:"two leaders`)
	applied := findGroup(s, `detsched: assertion failed: the check succeeds map[err:"node`)
	read := findGroup(s, "detsched: assertion failed: a read returns its own write")
	if len(s.groups) != 5 || first == nil || second == nil || first == second || leaders == nil || applied == nil || leaders == applied || read == nil || read.key != "detsched: assertion failed: a read returns its own write" {
		t.Fatalf("want a group for each invariant:\n%s", searchReport(s))
	}
	a := first.also[failureKey("detsched: assertion failed: the rare symptom is absent map[n:1]")]
	if a == nil || len(first.also) != 1 || len(second.also) != 0 {
		t.Fatalf("want the symptom in the first group only:\n%s", searchReport(s))
	}
	if k := s.kinds[failureKey(a.sig)]; k == nil || k.runs != a.runs || s.kinds[first.key].runs != first.runs {
		t.Errorf("want the runs that failed each way counted:\n%s", searchReport(s))
	}
	b, err := os.ReadFile(a.log)
	if err != nil || !bytes.Contains(b, []byte("\ndetsched: assertion failed: the rare symptom is absent map[")) {
		t.Errorf("log of the symptom's first run %s: %v\n%.2000s", a.log, err, b)
	}
}

// TestSearchLeftover checks that a group of runs whose test body returned
// while goroutines of its bubble were blocked names those goroutines.
func TestSearchLeftover(t *testing.T) {
	s := runSearch(t, "../../testdata/bubbleleak", 20)
	const pkg = "github.com/ryanworl/go-detsched/misc/detsched/testdata/bubbleleak"
	if len(s.groups) != 1 || s.groups[0].left != "2 × "+pkg+".(*store).merger (created by "+pkg+".open)" {
		t.Fatalf("want the two merger goroutines named:\n%s", searchReport(s))
	}
}

// TestSearchTimeout checks that a search runs on past -timeout, forking
// stopped programs it keeps from before then.
func TestSearchTimeout(t *testing.T) {
	s := newTestSearch(t, "../../testdata/bugrarelock")
	s.workers, s.timeout = 4, time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.run(ctx, 0); err != nil {
		t.Fatalf("%v\n%s", err, searchReport(s))
	}
	if s.arms[armCell].launches == 0 {
		t.Errorf("no runs from cells:\n%s", searchReport(s))
	}
}

// TestSearchHung checks that a run still going when the search ends,
// after it may hang, is stopped for its stacks and recorded with a replay
// line rather than dropped.
func TestSearchHung(t *testing.T) {
	s := newTestSearch(t, "../../testdata/hang")
	s.workers, s.only, s.noFork = 2, armFresh, true
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := s.run(ctx, 0); err != nil {
		t.Fatalf("%v\n%s", err, searchReport(s))
	}
	g := findGroup(s, "did not finish: still running when the search ended")
	if g == nil {
		t.Fatalf("no group for runs still going:\n%s", searchReport(s))
	}
	if !strings.Contains(g.replay, "GODETSEED=") || !strings.Contains(g.replay, "stopat=") {
		t.Errorf("replay line %q, want one that stops at the switch the run was stopped at", g.replay)
	}
	if fs := s.fallbacks(); slices.ContainsFunc(fs, func(f fallback) bool { return strings.Contains(f.what, "still running") }) {
		t.Errorf("the group is reported again as a fallback:\n%s", searchReport(s))
	}
	log, err := os.ReadFile(g.logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte("main.main()")) {
		t.Errorf("log has no stack of main.main:\n%s", log)
	}
}

// TestSearchRunaway runs harnesses whose runs do not finish on every
// fourth seed: a livelock, where virtual time stays put; a poll that
// goes on forever as virtual time moves, in the timeline or in a branch
// of it, in a dependency's code, or in goroutines that exit; and a
// history that grows without bound. Before, each such run went on until -timeout or the search's
// end, and was reported as a group and again as a fallback. Each must
// now stop at a limit set from the passing runs, as one group that names
// the limit and where its switches went, in the code under test, with a
// replay line that stops at the same switch. A computation that takes 10 times longer on those
// seeds is none of these, and passes, as does one that takes 27 times
// longer than the first runs to pass, whose limits were set from them.
func TestSearchRunaway(t *testing.T) {
	for _, tc := range []struct{ test, sig, field, where, search string }{
		{"TestLivelock", "did not finish: livelock at t=0.000s: virtual time stayed put for ", "maxstuck=", "runaway.TestLivelock.func1.2 (runaway_test.go:", ""},
		{"TestPoll", "did not finish: ran ", "maxswitches=", "runaway.TestPoll.func1 (runaway_test.go:", ""},
		{"TestBranchPoll", "did not finish: ran ", "maxswitches=", "runaway.TestBranchPoll.func1 (runaway_test.go:", ""},
		{"TestDepSpin", "did not finish: ran ", "maxswitches=", "runaway.spin (runaway_test.go:", "perturb=off quantum=100"},
		{"TestChurn", "did not finish: ran ", "maxswitches=", "exited goroutines, started in runaway.TestChurn.func1.2", ""},
		{"TestHistory", "did not finish: memory: ", "maxheap=", "runaway.TestHistory.func1 (runaway_test.go:", ""},
		{"TestCompute", "", "", "", "perturb=off quantum=100"},
		{"TestLong", "", "", "", "perturb=off quantum=10"},
	} {
		t.Run(tc.test, func(t *testing.T) {
			s := newTestSearch(t, "../../testdata/runaway")
			s.workers, s.args = 4, []string{"-test.run=^" + tc.test + "$"}
			if tc.search != "" {
				s.env = []string{"GODETSEARCH=" + tc.search}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := s.run(ctx, 24); err != nil {
				t.Fatalf("%v\n%s", err, searchReport(s))
			}
			r := searchReport(s)
			if tc.sig == "" {
				if len(s.groups) > 0 {
					t.Errorf("want no failures:\n%s", r)
				}
				return
			}
			if len(s.groups) != 1 || !strings.HasPrefix(s.groups[0].sig, tc.sig) {
				t.Fatalf("want one group, %q...:\n%s", tc.sig, r)
			}
			g := s.groups[0]
			if g.status != "reproduced" || !strings.Contains(g.replay, tc.field) || !strings.Contains(g.where, tc.where) || len(g.also) > 0 {
				t.Errorf("want the replay, with %s, to reproduce it, and the switches to go to %s:\n%s", tc.field, tc.where, r)
			}
			// The build without optimizations has another heap, but the
			// same switches.
			if want := tc.field; want == "maxheap=" && (strings.Contains(g.debug, want) || !strings.Contains(g.debug, "stopat=")) || want != "maxheap=" && !strings.Contains(g.debug, want) {
				t.Errorf("debug line %q, want it to stop with %s, or stopat= for maxheap=", g.debug, want)
			}
			if slices.ContainsFunc(s.fallbacks(), func(f fallback) bool { return f.anchor != "debug-lines-run-the-optimized-binary" }) {
				t.Errorf("want no fallbacks:\n%s", r)
			}
		})
	}
}

// TestSearchBadHarness runs harnesses with mistakes that a search without
// failures used to hide, and checks that the summary names each:
// assertions no run reached, with assert.Unreachable for one whose
// condition is false, a Sometimes that never held, runs that end
// before any fault, and faults the harness cannot get; and that t.Fatal on
// a simulated node's goroutine fails the run there, naming the call,
// where before the test waited for good and the run ended as a deadlock;
// and that of the faults that come with a failure, the summary names the
// one it needs, where before it named those that fired more often in
// failing runs than in passing ones, such as clock skips that come in the
// same storms, also for a failure noticed long after its cause, which
// variants reseeded close to the failure all show, with or without it.
func TestSearchBadHarness(t *testing.T) {
	search := func(t *testing.T, test string, n uint64) *search {
		t.Setenv("TMPDIR", t.TempDir())
		s, err := newSearch("../../testdata/badharness", "", "^"+test+"$")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if s.debugDone != nil {
				<-s.debugDone
			}
		})
		s.workers = 4
		if err := s.run(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		return s
	}
	t.Run("TestCoverage", func(t *testing.T) {
		s := search(t, "TestCoverage", 200)
		r := searchReport(s)
		want := map[string]string{
			"assertions-never-reached": `no run reached 1 of the program's 4 assertions: always "a client past 3 responses keeps its connection" (coverage_test.go:`,
			"sometimes-never-true":     `"a client saw more than 3 responses" (coverage_test.go:`,
			"runs-end-before-faults":   "no fault was injected before the run ended",
			"faults-never-injected":    "faults the harness cannot get: nemesis kill, killprocess, stop (NemesisConfig.Restart is nil); nemesis link, partition (fewer than 2 nodes)",
		}
		for _, f := range s.fallbacks() {
			if w, ok := want[f.anchor]; ok && strings.Contains(f.what, w) {
				delete(want, f.anchor)
			}
		}
		if len(want) > 0 || len(s.groups) > 0 {
			t.Errorf("no failures and these warnings missing, %v:\n%s", want, r)
		}
	})
	t.Run("TestErrorBranch", func(t *testing.T) {
		s := search(t, "TestErrorBranch", 20)
		r := searchReport(s)
		if !strings.Contains(r, `no run reached 1 of the program's 2 assertions: always "the number parses" (unreachable_test.go:22; for a branch no run should reach, use assert.Unreachable)`) {
			t.Errorf("want the warning to suggest assert.Unreachable:\n%s", r)
		}
	})
	t.Run("TestFatalOnNode", func(t *testing.T) {
		s := search(t, "TestFatalOnNode", 8)
		r := searchReport(s)
		g := findGroup(s, "fatal error: detsched: t.FailNow or t.SkipNow on a goroutine that is not the test's")
		if len(s.groups) != 1 || g == nil || g.status != "reproduced" {
			t.Fatalf("want one group for t.Fatal on a node's goroutine, reproduced:\n%s", r)
		}
		log, err := os.ReadFile(g.logs[0])
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`detsched: \S*badharness\.TestFatalOnNode\.func1\.3 at \S*/fatal_test\.go:\d+ called t\.FailNow`).Match(log) {
			t.Errorf("log does not name the call:\n%s", log)
		}
	})
	t.Run("TestLostUpdate", func(t *testing.T) {
		s := search(t, "TestLostUpdate", 60)
		r := searchReport(s)
		if len(s.groups) != 1 || s.groups[0].status != "reproduced" {
			t.Fatalf("want one group, reproduced:\n%s", r)
		}
		if !regexp.MustCompile(`^needs: sim\.net\.loss \(0 of \d+ fail without it\); \d+ of \d+ fail with every fault, in variants of its first run reseeded at its start$`).MatchString(s.groups[0].attrib) {
			t.Errorf("want the group to need loss:\n%s", r)
		}
	})
	t.Run("TestUnsyncedDir", func(t *testing.T) {
		s := search(t, "TestUnsyncedDir", 60)
		r := searchReport(s)
		if len(s.groups) != 1 || s.groups[0].status != "reproduced" {
			t.Fatalf("want one group, reproduced:\n%s", r)
		}
		if !regexp.MustCompile(`runs that reached each fault assertion: [^\n]*sim\.disk\.strictdirsync \d`).MatchString(r) || !strings.HasPrefix(s.groups[0].attrib, "needs") || !strings.Contains(s.groups[0].attrib, "sim.disk.strictdirsync") {
			t.Errorf("want the fault counted, and the group to need it:\n%s", r)
		}
		// A World reads TMPDIR, which a new shell has.
		if strings.Contains(s.groups[0].replay, "TMPDIR") {
			t.Errorf("replay line %s sets TMPDIR", s.groups[0].replay)
		}
	})
	t.Run("TestSlowServer", func(t *testing.T) {
		s := search(t, "TestSlowServer", 40)
		r := searchReport(s)
		if len(s.groups) != 1 || s.groups[0].status != "reproduced" {
			t.Fatalf("want one group, reproduced:\n%s", r)
		}
		needs, not, _ := strings.Cut(s.groups[0].attrib, "; not needed: ")
		if !strings.HasPrefix(needs, "needs: nemesis pause (0 of 8 fail without it)") || strings.Contains(needs, "clockskip") || !strings.Contains(not, "nemesis clockskip (") || strings.Contains(r, "faults in its runs") {
			t.Errorf("want the group to need pauses, and not clock skips:\n%s", r)
		}
	})
}

// TestSearchReports checks that the search prints where seed 1 leaves the
// deterministic subset: here, by starting a subprocess, at a switch and
// tick of the run.
func TestSearchReports(t *testing.T) {
	s := newTestSearch(t, "../../testdata/subprocess")
	var b strings.Builder
	s.progress = &b
	if err := s.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	want := regexp.MustCompile(`\nWARNING: \d+ fallbacks? limited this search.*\n  - seed 1 left the deterministic subset in 1 places, first: goroutine 1 starts a subprocess \(\S*/true\), whose timing comes from the OS, at s\d+ k\d+\n    cost: .*\n    fix:  .*seed-1\.log\n    docs: .*README\.md#seed-1-left-the-deterministic-subset\n`)
	if r := searchReport(s); len(s.left) != 1 || !want.MatchString(r) {
		t.Errorf("%d reports, summary:\n%s\nwant 1, matching %s", len(s.left), r, want)
	}
	if log, err := os.ReadFile(s.temp("seed-1.log")); err != nil || !bytes.Contains(log, []byte("syscall.forkExec")) {
		t.Errorf("seed-1.log has no stack of the report: %v\n%s", err, log)
	}
}

// TestSearchCompose checks that the search finds simcompose's seeded bug,
// and that each failure's command replays it in a new shell, in place of
// the composer branch it failed in, setting the variable that plants the
// bug and no other, and unsetting the one the program read but the search
// did not have, which would change the schedule, and not one whose
// presence changes nothing.
func TestSearchCompose(t *testing.T) {
	t.Setenv("SIMCOMPOSE_BUG", "1")
	t.Setenv("SIMCOMPOSE_UNREAD", "1")
	s := newTestSearch(t, "../../testdata/simcompose")
	// Plain runs of fresh seeds, so that the same seeds run every time:
	// the default mix depends on timing and sometimes misses the bug in
	// 60 runs. Seeds 2 to 60 fail 7 times on linux/amd64.
	s.workers, s.only, s.noFork = 4, armFresh, true
	if err := s.run(context.Background(), 60); err != nil {
		t.Fatal(err)
	}
	if len(s.groups) == 0 {
		t.Fatalf("no failure:\n%s", searchReport(s))
	}
	if !slices.Equal(s.vars, []string{"SIMCOMPOSE_BUG=1"}) {
		t.Errorf("replay lines set %q, want SIMCOMPOSE_BUG=1", s.vars)
	}
	for _, g := range s.groups {
		if g.status != "reproduced" {
			t.Errorf("%s: the replay %s", g.sig, g.status)
		}
		if !strings.Contains(g.replay, " env -u SIMCOMPOSE_EXTRA ") || strings.Contains(g.replay, "SIMCOMPOSE_UNUSED") {
			t.Errorf("%s: replay %s does not unset SIMCOMPOSE_EXTRA, or unsets SIMCOMPOSE_UNUSED, which changes nothing", g.sig, g.replay)
		}
	}
	cmd := exec.Command("sh", "-c", s.groups[0].replay)
	cmd.Env = append(os.Environ(), "SIMCOMPOSE_EXTRA=1")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), s.groups[0].sig) || strings.Contains(string(out), "warning: this run's schedule is not") {
		t.Errorf("with SIMCOMPOSE_EXTRA=1, %s printed\n%.3000s\nwant %s on the same schedule", s.groups[0].replay, out, s.groups[0].sig)
	}

	// The debug line runs the build without optimizations, which fails
	// the same way on the same schedule.
	<-s.debugDone
	debug := s.debugLine(s.groups[0])
	if s.debugBin == "" || !strings.Contains(debug, "dlv exec --disable-aslr "+s.debugBin) {
		t.Fatalf("debug line %s does not run the build without optimizations (%s)", debug, s.debugNote)
	}
	out, _ = exec.Command("sh", "-c", strings.Replace(debug, "dlv exec --disable-aslr ", "", 1)).CombinedOutput()
	if !strings.Contains(string(out), s.groups[0].sig) || strings.Contains(string(out), "warning: this run's schedule is not") {
		t.Errorf("%s printed\n%.3000s\nwant %s on the same schedule", debug, out, s.groups[0].sig)
	}
}

// TestSearchSeedOneBranches checks that the runs of seed 1 that the search
// compares with the base run, which runs the timeline without its
// composer branches, run without them too (testdata/simbranchstop, whose
// branches all fail): with them, a failing branch's trace line comes
// first, and the search said that seed 1 ran two different schedules and
// that debug lines run the optimized binary.
func TestSearchSeedOneBranches(t *testing.T) {
	s := newTestSearch(t, "../../testdata/simbranchstop")
	s.workers, s.only, s.noFork = 2, armFresh, true
	if err := s.run(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	<-s.debugDone
	if s.unstable != "" || s.debugBin == "" {
		t.Errorf("seed 1 rechecked as unstable (%q), or debug lines kept the optimized binary (%s):\n%s", s.unstable, s.debugNote, searchReport(s))
	}
}

// TestSearchSeedOneCut checks that runs of seed 1 stopped at -timeout,
// whose trace lines are those of where each was stopped, are not compared
// as if they had run to their ends (testdata/memstop): before, two runs of
// a deterministic program, started at once and stopped at different
// switches, made the search say that seed 1 ran two different schedules.
func TestSearchSeedOneCut(t *testing.T) {
	s := newTestSearch(t, "../../testdata/memstop")
	s.workers, s.only, s.noFork, s.timeout = 1, armFresh, true, 100*time.Millisecond
	if err := s.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if s.unstable != "" {
		t.Errorf("runs stopped at -timeout compared as complete: %s", s.unstable)
	}
}

// TestSearchSeedOneDivergence checks that when two runs of seed 1 run
// different schedules (testdata/pidsched, whose schedule depends on its
// process ID), the search says where two more runs with
// GODEBUG=dettrace=2 first differ, which goroutines ran and where the one
// before stopped, and keeps a window of each trace around the difference,
// with its final trace line, but not the whole traces.
func TestSearchSeedOneDivergence(t *testing.T) {
	s := newTestSearch(t, "../../testdata/pidsched")
	s.workers, s.only, s.noFork = 1, armFresh, true
	if err := s.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^seed 1 ran two different schedules in two runs started at once: .*; detsched started them alike, but for the file the first wrote its feedback to; two more runs of it with GODEBUG=dettrace=2 first differ at switch (\d+) \(k\d+ to goroutine \d+ in the first, k\d+ to goroutine \d+ in the second\), after goroutine \d+ (stopped|was preempted) in main\.main\S* \S+/main\.go:\d+ in the first and .* in the second \((\S+), (\S+)\)$`).FindStringSubmatch(s.unstable)
	if m == nil {
		t.Fatalf("the warning does not say where the runs first differ: %q", s.unstable)
	}
	// Each run's window around the difference, which it names, with its
	// final trace line. Where the runs differ depends on their process
	// IDs, but it is in the window's first 3000 lines.
	for _, path := range m[3:] {
		b, err := os.ReadFile(path)
		lines := strings.Split(string(b), "\n")
		var at int
		_, serr := fmt.Sscanf(lines[0], "# lines 1 to %d of the output with GODEBUG=dettrace=2, with the first difference at line %d", new(int), &at)
		if err != nil || serr != nil || at >= len(lines) || !strings.HasPrefix(lines[at], "detsched: switch "+m[1]+" ") || !traceLine.Match(b) {
			t.Errorf("window %s: %v, or its header does not name the line of switch %s, or no trace line:\n%.500s", path, err, m[1], b)
		}
		if n := bytes.Count(b, []byte("\n")); n > windowBefore+windowAfter+3 {
			t.Errorf("window %s has %d lines, the whole trace or most of it", path, n)
		}
	}
	if full, _ := filepath.Glob(s.temp("*.full*")); len(full) > 0 {
		t.Errorf("whole traces left in the out directory: %q", full)
	}
}

// TestSearchMemStoppedSeedOne checks that a run of seed 1 that guardMem
// stops when memory runs low, in recheck or buildDebug, counts as no
// answer: before, its cut trace line made the search say that seed 1 ran
// two different schedules, and that the build without optimizations ran
// another one. testdata/memstop computes long enough to be stopped.
func TestSearchMemStoppedSeedOne(t *testing.T) {
	s := newTestSearch(t, "../../testdata/memstop")
	s.workers, s.only, s.noFork = 1, armFresh, true
	if err := s.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	<-s.debugDone
	if s.unstable != "" || s.debugBin == "" {
		t.Fatalf("with memory to spare, seed 1 rechecked as unstable (%q), or debug lines kept the optimized binary (%s)", s.unstable, s.debugNote)
	}
	// Now memory is low, as guardMem sees it, and it stops every run.
	defer func(f func() (uint64, uint64)) { readMemory = f }(readMemory)
	readMemory = func() (uint64, uint64) { return 1, 100 }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.guardMem(ctx)
	s.recheck()
	s.debugBin, s.debugDone = "", make(chan bool)
	os.RemoveAll(s.temp("debug"))
	s.buildDebug(ctx)
	if s.unstable != "" || !strings.Contains(s.debugNote, "memory ran low") {
		t.Errorf("seed 1 rechecked as unstable (%q), or the debug note does not say that memory ran low: %s", s.unstable, s.debugNote)
	}
}

// TestSearchBranchLimit checks that a run whose first branch fails, and
// whose second then stops at the livelock limit, after the timeline's
// output (testdata/simbranchlimit), replays the first: the failure the
// search reports is the first branch's, which comes first. Before, the
// replay line took the second branch's number and trace, from the last
// "did not finish" in the output, without its limits, and ran it to a
// pass.
func TestSearchBranchLimit(t *testing.T) {
	s := newTestSearch(t, "../../testdata/simbranchlimit")
	s.workers, s.only, s.noFork = 2, armFresh, true
	if err := s.run(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	g := findGroup(s, "detsched: assertion failed: the first branch's check holds")
	if g == nil || len(s.groups) != 1 {
		t.Fatalf("want one group, for the first branch:\n%s", searchReport(s))
	}
	log, err := os.ReadFile(g.logs[0])
	if err != nil {
		t.Fatal(err)
	}
	trace := traceHash.FindSubmatch(log)
	if !bytes.Contains(log, []byte("detsched: branch 2 (GODETBRANCH=2 runs it in place)")) || trace == nil {
		t.Fatalf("the run did not stop the second branch at a limit after the first branch's failure:\n%s", log)
	}
	if g.status != "reproduced" || !strings.Contains(g.replay, " GODETBRANCH=1 ") || !strings.Contains(g.replay, "trace="+string(trace[1])) {
		t.Errorf("replay %s %s, want GODETBRANCH=1 and the first branch's trace=%s", g.replay, g.status, trace[1])
	}
}

// TestSearchTwoTests checks that a search of a package whose tests each
// complete setup, of which only the first is a fork point, says to search
// each test on its own.
func TestSearchTwoTests(t *testing.T) {
	s := newTestSearch(t, "../../testdata/twotests")
	if err := s.run(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(s.fallbacks(), func(f fallback) bool {
		return f.anchor == "tests-share-a-fork-point" && strings.Contains(f.what, "tests after TestFirst complete setup in the same process (TestSecond)") && strings.Contains(f.fix, "-run '^TestFirst$'")
	}) {
		t.Errorf("summary does not say to search each test on its own:\n%s", searchReport(s))
	}
}

// TestEnvScan checks that a program that ranges over its environment
// replays its failures through their replay lines, run from the shell
// that ran the search: the lines set GODETBRANCH and another GODETSEARCH
// than the search's runs had, and the program must not see the variables
// that control det mode (testdata/envscan). A new shell, with other
// variables, runs another schedule, which the search reports.
func TestEnvScan(t *testing.T) {
	s := runSearch(t, "../../testdata/envscan", 20)
	if len(s.groups) == 0 {
		t.Fatalf("want failures:\n%s", searchReport(s))
	}
	for _, g := range s.groups {
		out, _ := exec.Command("sh", "-c", g.replay).CombinedOutput()
		trace := regexp.MustCompile(`trace=(0x[0-9a-f]+)`).FindStringSubmatch(g.replay)
		if trace == nil || !bytes.Contains(out, []byte("detsched: trace="+trace[1]+" ")) {
			t.Errorf("group %d's replay line ran another schedule than the run's (%v):\n%s\n%s", g.index, trace, g.replay, out)
		}
	}
}

// TestSearchDebugFallback checks that debug lines keep the optimized
// binary, and the summary says where the builds part, when the build
// without optimizations runs another schedule: testdata/stackread's
// schedule depends on its own stacks.
func TestSearchDebugFallback(t *testing.T) {
	s := newTestSearch(t, "../../testdata/stackread")
	if err := s.run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	<-s.debugDone
	if len(s.groups) != 1 {
		t.Fatalf("want one failure:\n%s", searchReport(s))
	}
	if debug := s.debugLine(s.groups[0]); s.debugBin != "" || !strings.Contains(debug, "dlv exec --disable-aslr "+quote(s.bin)) {
		t.Errorf("debug line %s, want the optimized binary %s", debug, s.bin)
	}
	if r := searchReport(s); !strings.Contains(r, "debug lines run the optimized binary") || !strings.Contains(r, "ran seed 1 on another schedule") ||
		!regexp.MustCompile(`the builds first differ at switch \d+ \(k\d+( to goroutine \d+)? optimized, k\d+( to goroutine \d+)? without optimizations\), after goroutine \d+ ((was preempted|stopped) in \S+ \S+:\d+|exited) optimized and goroutine \d+ ((was preempted|stopped) in \S+ \S+:\d+|exited) without`).MatchString(r) {
		t.Errorf("summary does not say why debug lines run the optimized binary, and where the builds part:\n%s", r)
	}
}

// TestSearchDrift checks that the search finds simdrift's lease that a
// server and its holder measure on their own clocks, which breaks in the
// runs where the clock rates the nodes draw when setup completes put the
// holder's more than 1% behind the server's: the runs it forks after
// setup draw their own rates.
func TestSearchDrift(t *testing.T) {
	t.Setenv("SIMDRIFT_DRAWN", "1")
	t.Setenv("TMPDIR", t.TempDir())
	s, err := newSearch("../../testdata/simdrift", "", "^TestDrawn$")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s.debugDone != nil {
			<-s.debugDone
		}
	})
	s.workers = 4
	if err := s.run(context.Background(), 200); err != nil {
		t.Fatal(err)
	}
	g := findGroup(s, "TestDrawn")
	if g == nil || g.status != "reproduced" {
		t.Fatalf("want the lease broken, reproduced:\n%s", searchReport(s))
	}
	if log, err := os.ReadFile(g.logs[0]); err != nil || !bytes.Contains(log, []byte("two clients held the lease")) {
		t.Errorf("the failure's log does not say the lease broke: %v\n%s", err, log)
	}
	// The same run, with the drift turned off, passes.
	off := regexp.MustCompile(`trace=0x[0-9a-f]+`).ReplaceAllString(g.replay, "faultoff=sim.clock.drift")
	if out, err := exec.Command("sh", "-c", off).CombinedOutput(); err != nil || !strings.Contains(off, "faultoff=") {
		t.Errorf("%s: %v, want a pass without the drift\n%s", off, err, out)
	}
}

// TestSearchSetupRoots checks that each setup root stops right after its
// own seed's setup, not seed 1's, so that the runs forked from it each
// draw what setup's end draws. setupdraw's setup takes longer in seed 1
// than in most seeds, and fails 1 in 16 runs on a draw at setup's end;
// roots stopped at seed 1's setup's end left each root's runs with one
// draw, so that they failed all together or not at all.
func TestSearchSetupRoots(t *testing.T) {
	s := runSearch(t, "../../testdata/setupdraw", 256)
	if s.failed < 4 || s.failed > 48 {
		t.Errorf("%d of %d runs failed, want about 1 in 16:\n%s", s.failed, s.runs, searchReport(s))
	}
	g := findGroup(s, "TestDraw")
	if g == nil {
		t.Fatalf("no failure:\n%s", searchReport(s))
	}
	m := regexp.MustCompile(`GODETSEED=(\d+) .*tape=(\d+):`).FindStringSubmatch(g.replay)
	if m == nil {
		t.Fatalf("want a run forked from a setup root: %s", g.replay)
	}
	out, _ := s.plain(atou(m[1])).CombinedOutput()
	if setup := setupLine.FindSubmatch(out); setup == nil || atou(m[2]) != atou(string(setup[1]))+1 {
		t.Errorf("the run forked at switch %s, want right after its seed's own setup (%q):\n%s", m[2], setup, g.replay)
	}
}

// TestSearchShard checks one shard of a search spread over machines: it
// runs a prebuilt binary in the current directory, from fresh seeds past
// -seed-base, and each failure's command replays it with that seed and a
// copy of the binary, which replays it after the binary is gone.
func TestSearchShard(t *testing.T) {
	t.Setenv("SIMCOMPOSE_BUG", "1")
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir("../../testdata/simcompose")
	bin := filepath.Join(t.TempDir(), "simcompose.test")
	build := exec.Command(goCmd(), "test", "-c", "-o", bin)
	build.Env = append(os.Environ(), "GOEXPERIMENT=detsched", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s, err := newSearch("", bin, "")
	if err != nil {
		t.Fatal(err)
	}
	const base = 1 << 40
	s.workers, s.only, s.noFork, s.seedBase = 4, armFresh, true, base
	if err := s.run(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if len(s.groups) == 0 {
		t.Fatalf("no failure:\n%s", searchReport(s))
	}
	seed := regexp.MustCompile(`GODETSEED=(\d+) `)
	for _, g := range s.groups {
		m := seed.FindStringSubmatch(g.replay)
		n, _ := strconv.ParseUint(m[1], 10, 64)
		// Seed 1 is every shard's base run.
		if n != 1 && (n <= base+1 || n > base+100) || !strings.HasSuffix(g.replay, " "+s.bin) || s.bin == bin {
			t.Errorf("%s: replay %s, want seed 1 or a seed from %d to %d, and a copy of %s", g.sig, g.replay, base+2, base+100, bin)
		}
		if g.status != "reproduced" {
			t.Errorf("%s: the replay %s", g.sig, g.status)
		}
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("sh", "-c", s.groups[0].replay).CombinedOutput()
	if !strings.Contains(string(out), s.groups[0].sig) {
		t.Errorf("with the binary removed, %s printed\n%s\nwant %s", s.groups[0].replay, out, s.groups[0].sig)
	}
}

func findGroup(s *search, sig string) *group {
	for _, g := range s.groups {
		if strings.Contains(g.sig, sig) {
			return g
		}
	}
	return nil
}

// branchOutcome forks a child at s.at with the given seed and PCT
// horizon, waits for it, and returns its outcome.
func (s *forkServer) branchOutcome(seed, steps uint64) (outcome, error) {
	stdout, err := os.CreateTemp("", "detfork")
	if err != nil {
		return outcome{}, err
	}
	defer os.Remove(stdout.Name())
	defer stdout.Close()
	stderr, err := os.CreateTemp("", "detfork")
	if err != nil {
		return outcome{}, err
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()
	pid, _, err := s.branch(seed, steps, branchFiles{stdout: stdout, stderr: stderr})
	if err != nil {
		return outcome{}, err
	}
	code, err := s.wait(pid)
	if err != nil {
		return outcome{}, err
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		return outcome{}, err
	}
	errOut, err := os.ReadFile(stderr.Name())
	if err != nil {
		return outcome{}, err
	}
	return newOutcome(string(out), string(errOut), code), nil
}

// TestMutationCheck runs -check on harnesses of one package: a strong one
// that tests the code concurrently and with failing syncs, which must
// catch a dropped lock, every lock of a mutex locked and unlocked in
// different methods dropped, and an ignored error, and a weak one that
// calls it from one goroutine, whose clean searches prove nothing, which
// must let the lock survive and get no mutant on an error path it never
// takes. A mutant that does not compile is skipped, and a
// harness that fails without mutations must be fixed first.
func TestMutationCheck(t *testing.T) {
	check := func(t *testing.T, pkg, test string) (int, string) {
		t.Setenv("TMPDIR", t.TempDir())
		var b strings.Builder
		code, err := mutationCheck(context.Background(), pkg, "^"+test+"$", 40, 4, time.Minute, &b)
		if err != nil {
			t.Fatal(err)
		}
		return code, b.String()
	}
	t.Run("TestStrong", func(t *testing.T) {
		code, out := check(t, "../../testdata/mutate", "TestStrong")
		for _, want := range []string{
			"(1 more did not compile and was skipped)",
			"drop the Lock and Unlock of c.mu: detsched: assertion failed: the count is the sum of the adds",
			"ignore the error in err != nil: detsched: assertion failed: the log counts the syncs that succeeded",
			"drop w.Sync(), as if it succeeded: detsched: assertion failed: the log counts the syncs that succeeded",
			// Locked in Begin and unlocked in Commit.
			"drop every Lock and Unlock of writer, 4 calls: detsched: assertion failed: the store's value is the sum of the commits",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("exit status %d, want %q in:\n%s", code, want, out)
			}
		}
	})
	t.Run("TestWeak", func(t *testing.T) {
		code, out := check(t, "../../testdata/mutate", "TestWeak")
		for _, want := range []string{
			"caught 2 of 10 mistakes",
			// Named by its path in its module, as a module can have
			// many store.go files.
			"survived: testdata/mutate/store.go:36 drop the Lock and Unlock of c.mu",
		} {
			if code != 1 || !strings.Contains(out, want) {
				t.Errorf("exit status %d, want 1 and %q in:\n%s", code, want, out)
			}
		}
		// Its syncs never fail, so ignoring their errors changes nothing.
		if strings.Contains(out, "ignore the error") {
			t.Errorf("planted a mutant on an error path no run took:\n%s", out)
		}
	})
	// A harness module that tests a local copy of the code through
	// go.mod's replace, as harness.md recommends, which -check used to
	// report as a package without tests.
	t.Run("Replace", func(t *testing.T) {
		t.Chdir("../../testdata/checkreplace/harness")
		code, out := check(t, ".", "TestCounter")
		if want := "counter.go:26 drop the Lock and Unlock of c.mu: detsched: assertion failed: the count is the sum of the adds"; !strings.Contains(out, "caught:   "+want) {
			t.Errorf("exit status %d, want %q in:\n%s", code, want, out)
		}
	})
	t.Run("NothingToMutate", func(t *testing.T) {
		code, out := check(t, "../../testdata/badharness", "TestCoverage")
		if want := "no code to mutate: the test binary links no non-test Go files of github.com/ryanworl/go-detsched/misc/detsched (the tests' module)"; code != 2 || !strings.Contains(out, want) {
			t.Errorf("exit status %d, want 2 and %q in:\n%s", code, want, out)
		}
	})
	t.Run("TestFatalOnNode", func(t *testing.T) {
		code, out := check(t, "../../testdata/badharness", "TestFatalOnNode")
		if code != 1 || !strings.Contains(out, "the program fails without mutations; fix these failures first") || strings.Contains(out, "caught") {
			t.Errorf("exit status %d, want 1 and no mutants:\n%s", code, out)
		}
	})
}
