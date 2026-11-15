// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
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
			s, err := startForkServer(command(bin, env...))
			if err != nil {
				t.Fatalf("%s %s: %v", pkg, godebug, err)
			}
			compared := 0
			// At switch 2 the runtime's init runs with the P on another
			// thread, so that stop waits for it to come back.
			for _, n := range []uint64{1, 2, 7, 30} {
				// The server stops first at switch 1, and a run to a
				// switch at or before the current one runs to the end.
				if n != s.at {
					ok, err := s.run(n)
					if err != nil {
						t.Fatalf("%s %s: run to %d: %v", pkg, godebug, n, err)
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
							t.Errorf("%s %s: branch at %d (stopped at 2): %v", pkg, godebug, s.at, err)
						}
						// Such as with an M in a system call.
						t.Logf("%s %s: branch at %d: %v", pkg, godebug, n, err)
						break
					}
					if err != nil {
						t.Fatalf("%s %s: branch at %d: %v", pkg, godebug, n, err)
					}
					// A stop waits for the P to be on the main thread, so
					// it may be after n.
					tape := fmt.Sprintf("GODETSEARCH=tape=%d:%d", s.at, seed)
					if steps > 0 {
						tape += fmt.Sprintf(":%d", steps)
					}
					want, _, err := runBin(bin, append(env, tape)...)
					if err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Errorf("%s %s %s: forked child differs from re-execution:\nforked: %+v\nre-run: %+v", pkg, godebug, tape, got, want)
					}
					compared++
				}
			}
			s.close()
			if compared == 0 {
				t.Errorf("%s %s: no branch was compared", pkg, godebug)
			}
		}
	}
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
// network poller refuses to fork, and that it continues normally after.
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
	var forked, refused int
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
		case errors.As(err, &r) && r.reason == 4:
			refused++
		case err != nil:
			t.Fatalf("branch at %d: %v", n, err)
		case o.exitCode != 0 || !strings.HasSuffix(o.stdout, "done\n"):
			t.Errorf("branch at %d: %+v", n, o)
		default:
			forked++
		}
	}
	if forked == 0 || refused == 0 {
		t.Errorf("forked %d times and refused %d times; want both", forked, refused)
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
// program's setup, and that an assertion failure counts as a failure that
// its command replays.
func TestSearchSetup(t *testing.T) {
	s := runSearch(t, "../../testdata/sdk", 200)
	g := findGroup(s, "detsched: assertion failed: no increment is lost")
	if s.stop < 2 || g == nil || g.status != "reproduced" || g.runs == s.runs {
		t.Errorf("want some but not all runs failing the assertion, replayed:\n%s", searchReport(s))
	}
}

// TestSearchCompose checks that the search finds simcompose's seeded bug,
// and that each failure's command replays it in a new shell, in place of
// the composer branch it failed in, setting the variable that plants the
// bug and no other.
func TestSearchCompose(t *testing.T) {
	t.Setenv("SIMCOMPOSE_BUG", "1")
	t.Setenv("SIMCOMPOSE_UNREAD", "1")
	s := runSearch(t, "../../testdata/simcompose", 60)
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
