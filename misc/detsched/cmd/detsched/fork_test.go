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
	g := findGroup(s, "still running when the search ended")
	if g == nil {
		t.Fatalf("no group for runs still going:\n%s", searchReport(s))
	}
	if !strings.Contains(g.replay, "GODETSEED=") {
		t.Errorf("replay line %q", g.replay)
	}
	log, err := os.ReadFile(g.logs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte("main.main()")) {
		t.Errorf("log has no stack of main.main:\n%s", log)
	}
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
// did not have, which would change the schedule.
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
		if !strings.Contains(g.replay, " env -u SIMCOMPOSE_EXTRA ") {
			t.Errorf("%s: replay %s does not unset SIMCOMPOSE_EXTRA", g.sig, g.replay)
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

// TestSearchDebugFallback checks that debug lines keep the optimized
// binary, and the summary says why, when the build without optimizations
// runs another schedule: testdata/stackread's schedule depends on its own
// stacks.
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
	if r := searchReport(s); !strings.Contains(r, "debug lines run the optimized binary") || !strings.Contains(r, "ran seed 1 on another schedule") {
		t.Errorf("summary does not say why debug lines run the optimized binary:\n%s", r)
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
		if n <= base+1 || n > base+100 || !strings.HasSuffix(g.replay, " "+s.bin) || s.bin == bin {
			t.Errorf("%s: replay %s, want a seed from %d to %d and a copy of %s", g.sig, g.replay, base+2, base+100, bin)
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
