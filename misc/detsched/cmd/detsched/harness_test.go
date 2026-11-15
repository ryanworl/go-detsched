// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

// Helpers for the tests: running a program many times and comparing what
// it did, which is how the tests check that the runtime is deterministic,
// and running a search.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type config struct {
	runs     int
	parallel int
}

// An outcome is everything a run produced that should be the same in
// every run.
type outcome struct {
	trace    string // schedule hash and switch count printed by the runtime
	stdout   string
	stderr   string // without the trace line
	exitCode int
}

var testDuration = regexp.MustCompile(`(?m)^(\s*--- (?:PASS|FAIL|SKIP): \S+ )\(\d+\.\d+s\)`)

// newOutcome returns the outcome of a run that wrote stdout and stderr
// and exited with exitCode, moving the trace line out of stderr. Package
// testing prints how long each test took in real time, so the durations
// become "(?s)".
func newOutcome(stdout, stderr string, exitCode int) outcome {
	stdout = testDuration.ReplaceAllString(stdout, "$1(?s)")
	o := outcome{stdout: stdout, stderr: stderr, exitCode: exitCode}
	if m := traceLine.FindStringIndex(o.stderr); m != nil {
		o.trace = strings.TrimPrefix(o.stderr[m[0]:m[1]], "detsched: ")
		o.stderr = o.stderr[:m[0]] + strings.TrimPrefix(o.stderr[m[1]:], "\n")
	} else {
		o.trace = "no trace"
	}
	return o
}

// build builds pkg in a new temporary directory and returns the binary
// and a function that removes the directory.
func build(cfg config, pkg string) (bin string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "detsched-test")
	if err != nil {
		return "", nil, err
	}
	_, bin, err = buildPackage(pkg, tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	return bin, func() { os.RemoveAll(tmp) }, nil
}

// command returns a command that runs bin with env added to the
// environment, its GODEBUG settings after the inherited ones and
// dettrace=1, which prints the trace line also without GODETSEED, and
// the fields of its GODETSEARCH settings merged.
func command(bin string, env ...string) *exec.Cmd {
	godebug := os.Getenv("GODEBUG") + ",dettrace=1"
	var added, search []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GODEBUG="); ok {
			godebug += "," + v
		} else if v, ok := strings.CutPrefix(e, "GODETSEARCH="); ok {
			search = append(search, v)
		} else {
			added = append(added, e)
		}
	}
	cmd := exec.Command(bin)
	cmd.Env = append(append(os.Environ(), added...), "GODEBUG="+strings.TrimPrefix(godebug, ","))
	cmd.Env = addSearch(cmd.Env, search...)
	return cmd
}

// run runs cmd and returns its outcome and wall time.
func run(cmd *exec.Cmd) (outcome, time.Duration, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start)
	exitCode := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return outcome{}, 0, err
		}
		exitCode = ee.ExitCode()
	}
	return newOutcome(stdout.String(), stderr.String(), exitCode), wall, nil
}

func runBin(bin string, env ...string) (outcome, time.Duration, error) {
	return run(command(bin, env...))
}

// runAll runs bin n times, cfg.parallel at a time, with env(i) added for
// run i.
func runAll(cfg config, bin string, n int, env func(i int) []string) ([]outcome, []time.Duration, error) {
	outs := make([]outcome, n)
	walls := make([]time.Duration, n)
	errs := make([]error, n)
	sem := make(chan struct{}, max(cfg.parallel, 1))
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			outs[i], walls[i], errs[i] = runBin(bin, env(i)...)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}
	return outs, walls, nil
}

// A checkResult is what cfg.runs runs of a program did.
type checkResult struct {
	pkg      string
	outcomes []outcome // distinct outcomes, in order of first appearance
	counts   []int
}

func (r *checkResult) deterministic() bool { return len(r.outcomes) == 1 }

func (r *checkResult) summary() string {
	if r.deterministic() {
		return fmt.Sprintf("%s: %s, exit status %d", r.pkg, r.outcomes[0].trace, r.outcomes[0].exitCode)
	}
	return fmt.Sprintf("%s: %d outcomes, differ in %s", r.pkg, len(r.outcomes), strings.Join(r.differences(), ", "))
}

// differences names the parts of the outcomes that are not all equal.
func (r *checkResult) differences() []string {
	var d []string
	for _, f := range []struct {
		name string
		get  func(outcome) any
	}{
		{"trace", func(o outcome) any { return o.trace }},
		{"stdout", func(o outcome) any { return o.stdout }},
		{"stderr", func(o outcome) any { return o.stderr }},
		{"exit status", func(o outcome) any { return o.exitCode }},
	} {
		for _, o := range r.outcomes[1:] {
			if f.get(o) != f.get(r.outcomes[0]) {
				d = append(d, f.name)
				break
			}
		}
	}
	return d
}

func (r *checkResult) details() string {
	var b strings.Builder
	for i, o := range r.outcomes {
		fmt.Fprintf(&b, "\toutcome %d (%d runs): %s, exit status %d\n\t\tstdout: %q\n\t\tstderr: %q\n", i+1, r.counts[i], o.trace, o.exitCode, truncate(o.stdout, 2000), truncate(o.stderr, 2000))
	}
	return b.String()
}

// check builds pkg and runs it cfg.runs times with the same environment.
func check(cfg config, pkg string) (*checkResult, error) {
	bin, cleanup, err := build(cfg, pkg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	outs, _, err := runAll(cfg, bin, max(cfg.runs, 2), func(int) []string { return nil })
	if err != nil {
		return nil, err
	}
	r := &checkResult{pkg: pkg}
	for _, o := range outs {
		if i := slices.Index(r.outcomes, o); i >= 0 {
			r.counts[i]++
		} else {
			r.outcomes = append(r.outcomes, o)
			r.counts = append(r.counts, 1)
		}
	}
	return r, nil
}

// An exploration is what runs of a program with seeds first, first+1, ...
// did.
type exploration struct {
	pkg       string
	n         int
	schedules int // distinct schedule hashes
	failures  map[string][]uint64
	failed    int
	repro     string // reproduces the first failure
	// reproduced reports whether running repro failed the same way.
	reproduced bool
}

func (x *exploration) report() string {
	return fmt.Sprintf("%s: %d seeds, %d failed, %d schedules, failures %v, repro %q (reproduced %v)", x.pkg, x.n, x.failed, x.schedules, x.failures, x.repro, x.reproduced)
}

// exploreSeeds builds pkg and runs it once with each of n seeds from
// first, with the perturbation strategy perturb (GODETSEARCH's perturb
// field: walk or pct), or the default's if it is "".
func exploreSeeds(cfg config, pkg string, first uint64, n int, perturb string) (*exploration, error) {
	bin, cleanup, err := build(cfg, pkg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	env := func(s uint64) []string {
		e := []string{fmt.Sprintf("GODETSEED=%d", s)}
		if perturb != "" {
			e = append(e, "GODETSEARCH=perturb="+perturb)
		}
		return e
	}
	outs, _, err := runAll(cfg, bin, n, func(i int) []string { return env(first + uint64(i)) })
	if err != nil {
		return nil, err
	}
	x := &exploration{pkg: pkg, n: n, failures: map[string][]uint64{}}
	traces := map[string]bool{}
	firstFail := -1
	for i, o := range outs {
		traces[o.trace] = true
		if o.exitCode == 0 {
			continue
		}
		r := result{output: []byte(o.stderr + "\n" + o.stdout), exitCode: o.exitCode}
		x.failures[r.signature()] = append(x.failures[r.signature()], first+uint64(i))
		x.failed++
		if firstFail < 0 {
			firstFail = i
		}
	}
	x.schedules = len(traces)
	if firstFail >= 0 {
		e := env(first + uint64(firstFail))
		x.repro = strings.Join(command(bin, e...).Env[len(os.Environ()):], " ")
		again, _, err := runBin(bin, e...)
		if err != nil {
			return nil, err
		}
		x.reproduced = again == outs[firstFail]
	}
	return x, nil
}

// runSearch runs the search on pkg for n runs, 4 at a time.
func runSearch(t *testing.T, pkg string, n uint64) *search {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	s, err := newSearch(pkg, "")
	if err != nil {
		t.Fatal(err)
	}
	s.workers = 4
	if err := s.run(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	return s
}

// searchReport returns s's summary.
func searchReport(s *search) string {
	var b strings.Builder
	s.summary(&b)
	return b.String()
}

var perturbLine = regexp.MustCompile(`(?m)^detsched: perturb (.*)$`)
