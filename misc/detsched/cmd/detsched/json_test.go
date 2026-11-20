// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestMain runs detsched's main instead of the tests when
// DETSCHED_TEST_MAIN is set, so that a test can run the command as a
// user does (see runCLI).
func TestMain(m *testing.M) {
	if os.Getenv("DETSCHED_TEST_MAIN") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runCLI runs detsched with args in dir, and returns its standard output
// and error and its exit status.
func runCLI(t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DETSCHED_TEST_MAIN=1", "TMPDIR="+t.TempDir())
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

// readEvents reads a -json file, checking that every line is an object
// with an event and a time.
func readEvents(t *testing.T, name string) []map[string]any {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e map[string]any
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%v: %s", err, sc.Bytes())
		}
		if _, ok := e["event"].(string); !ok || e["time"] == nil {
			t.Fatalf("an event without its name or time: %s", sc.Bytes())
		}
		events = append(events, e)
	}
	if len(events) < 2 || events[0]["event"] != "start" || events[len(events)-1]["event"] != "end" {
		t.Fatalf("want events from start to end, got %d events: %v", len(events), events)
	}
	return events
}

// ofKind returns the events named event.
func ofKind(events []map[string]any, event string) []map[string]any {
	var es []map[string]any
	for _, e := range events {
		if e["event"] == event {
			es = append(es, e)
		}
	}
	return es
}

// replayFrom returns the command a -json replay object describes: argv
// in cwd, with the environment's unset variables removed and its set
// ones set.
func replayFrom(t *testing.T, replay map[string]any) *exec.Cmd {
	t.Helper()
	var argv []string
	for _, a := range replay["argv"].([]any) {
		argv = append(argv, a.(string))
	}
	var unset []string
	for _, k := range replay["unset"].([]any) {
		unset = append(unset, k.(string))
	}
	env := slices.DeleteFunc(os.Environ(), func(e string) bool {
		k, _, _ := strings.Cut(e, "=")
		return slices.Contains(unset, k)
	})
	for k, v := range replay["set"].(map[string]any) {
		env = append(env, k+"="+v.(string))
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = replay["cwd"].(string), env
	return cmd
}

// TestJSON checks -json on a search with a known failure
// (testdata/bugcache): the events go from start, with the schema, to end,
// with the exit status; there is a run event for every run, and the
// group's replay, run from its fields alone, reproduces its first run's
// failure on its schedule, and is the line the text prints.
func TestJSON(t *testing.T) {
	events := filepath.Join(t.TempDir(), "events.jsonl")
	stdout, _, code := runCLI(t, "../../testdata/bugcache", "-n", "100", "-p", "4", "-json", events)
	if code != 1 {
		t.Fatalf("exit status %d, want 1:\n%s", code, stdout)
	}
	es := readEvents(t, events)
	start, end := es[0], es[len(es)-1]
	if start["schema"] != float64(1) || start["mode"] != "search" || start["binary_sha256"] == "" || start["detsched"] == "" {
		t.Errorf("start event %v, want schema 1, mode search, the binary's hash, and detsched's version", start)
	}
	runs := ofKind(es, "run")
	var failed int
	traces := map[float64]string{} // the trace of each failing seed
	for _, r := range runs {
		if r["result"] == "fail" {
			failed++
			traces[r["seed"].(float64)] = r["trace"].(string)
		} else if r["result"] != "pass" {
			t.Errorf("run %v, want it passed or failed", r)
		}
	}
	if end["exit_status"] != float64(1) || end["runs"] != float64(len(runs)) || end["failed"] != float64(failed) || failed == 0 || len(runs) < 100 {
		t.Errorf("end event %v, want exit status 1 and the counts of %d run events, %d failed", end, len(runs), failed)
	}
	groups := ofKind(es, "group")
	if len(groups) != 1 || end["signatures"] != float64(1) {
		t.Fatalf("group events %v, want 1", groups)
	}
	g := groups[0]
	if g["reproduced"] != true || !strings.Contains(stdout, "\treplay: "+g["replay_line"].(string)+"\n") {
		t.Errorf("group %v: want it reproduced, and its replay line in the summary:\n%s", g, stdout)
	}
	seed := g["first"].(map[string]any)["seed"].(float64)
	want, ok := traces[seed]
	if !ok {
		t.Fatalf("no failing run event of the group's first seed, %v", seed)
	}
	replay := g["replay"].(map[string]any)
	if replay["set"].(map[string]any)["GODETSEED"] != fmt.Sprint(uint64(seed)) {
		t.Errorf("replay %v does not set the first run's seed, %v", replay, seed)
	}
	out, err := replayFrom(t, replay).CombinedOutput()
	if _, ok := err.(*exec.ExitError); !ok || !strings.Contains(string(out), g["message"].(string)) || !strings.Contains(string(out), "detsched: trace="+want+" ") {
		t.Errorf("the replay from its fields (%v) printed\n%s\nwant %q on the schedule %s", err, out, g["message"], want)
	}
}

// TestSeeds checks -seeds with -json on testdata/bugcache: the runs are
// exactly the seeds from -seed-base+1 to -seed-base+n, each from its
// start, none forked or directed, and each failing seed fails the same
// way on the same schedule when run with GODETSEED alone.
func TestSeeds(t *testing.T) {
	const base, n = 1000, 40
	events := filepath.Join(t.TempDir(), "events.jsonl")
	stdout, _, code := runCLI(t, "../../testdata/bugcache", "-seeds", "-seed-base", fmt.Sprint(base), "-n", fmt.Sprint(n), "-p", "4", "-json", events)
	es := readEvents(t, events)
	start, end := es[0], es[len(es)-1]
	if start["mode"] != "seeds" || start["seed_base"] != float64(base) {
		t.Errorf("start event %v, want mode seeds and seed base %d", start, base)
	}
	runs := ofKind(es, "run")
	var seeds []int
	failed := 0
	for _, r := range runs {
		seeds = append(seeds, int(r["seed"].(float64)))
		if r["arm"] != "fresh" || r["forked"] != false || r["tape"] != nil || r["yield"] != nil {
			t.Errorf("run %v, want a fresh seed, neither forked nor directed", r)
		}
		if r["result"] != "fail" {
			continue
		}
		failed++
		cmd := exec.Command(start["binary"].(string))
		cmd.Dir = start["cwd"].(string)
		cmd.Env = append(os.Environ(), fmt.Sprintf("GODETSEED=%d", int(r["seed"].(float64))))
		out, err := cmd.CombinedOutput()
		if _, ok := err.(*exec.ExitError); !ok || !strings.Contains(string(out), r["signature"].(string)) || !strings.Contains(string(out), "detsched: trace="+r["trace"].(string)+" ") {
			t.Errorf("seed %v alone (%v) printed\n%s\nwant %q on the schedule %s", r["seed"], err, out, r["signature"], r["trace"])
		}
	}
	slices.Sort(seeds)
	want := make([]int, n)
	for i := range want {
		want[i] = base + 1 + i
	}
	if !slices.Equal(seeds, want) {
		t.Errorf("run events of seeds %v, want %d to %d, each once", seeds, base+1, base+n)
	}
	if failed == 0 || code != 1 || end["exit_status"] != float64(1) || end["runs"] != float64(n) || end["failed"] != float64(failed) {
		t.Errorf("exit status %d, end event %v, want 1 and %d runs, %d failed:\n%s", code, end, n, failed, stdout)
	}
	if !strings.Contains(stdout, fmt.Sprintf("detsched: runs started: %d seeds from %d, each from its start (-seeds)\n", n, base+1)) {
		t.Errorf("the summary does not say which seeds ran:\n%s", stdout)
	}
}
