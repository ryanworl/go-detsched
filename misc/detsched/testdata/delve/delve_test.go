// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package delve tests the Delve that the release bundles (built from
// misc/release/delve.bash) on a real failing replay: detsched finds the
// planted bug of testdata/simcompose with SIMCOMPOSE_BUG=1, and the tests
// run the debug: line it prints with scripts, as a user or an agent does.
// They check what the docs promise: the run's position on every hit, which
// the runtime's own GODETSTOP stop line confirms; goroutines by node;
// restart to a checkpoint, a position and rewind; the JSON output mode;
// the trace line at exit, unchanged by the breakpoints; and that the
// runtime-counted hit count stops at the hit stock Delve's counting does.
// On a small program of its own, promoted, they check that the hits of a
// method inlined into compiler-generated wrappers name the method, and
// that a conditional tracepoint's returns follow its condition and carry
// its name. On returns, they check that the results at a tracepoint's
// returns read in optimized code as in a build without optimizations, and
// that strings print whole. On replaced, a program using a module
// replaced by a local directory, they check locations: a file by its
// import path, a closure in a closure, a method inlined at an address
// another tracepoint has, the frames of goroutines -t in JSON, and that
// a breakpoint's frames have the stack record's shape. TestNodeAndStart
// checks detsched.node and detsched.start, and on values, a plain program,
// TestValues checks how channels print and that on ... print is labelled
// with its expression.
//
// $DLV names the dlv to test; the default is dlv next to go. gate.bash
// runs them when the tree has bin/dlv:
//
//	cd misc/detsched && go test -count=1 ./testdata/delve
package delve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// failure is the replay of the planted bug detsched found.
type failure struct {
	replay string // the replay: line
	debug  string // the debug: line
	trace  string // the run's trace line
	starts map[string]bool
}

var (
	failOnce sync.Once
	fail     failure
	failErr  error
)

// findFailure runs detsched on testdata/simcompose with its bug planted
// and returns the replay of a failure it found.
func findFailure(t *testing.T) *failure {
	t.Helper()
	failOnce.Do(func() {
		dir, err := filepath.Abs("../simcompose")
		if err != nil {
			failErr = err
			return
		}
		cmd := exec.Command("detsched", "-n", "100")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SIMCOMPOSE_BUG=1")
		out, _ := cmd.CombinedOutput()
		last := func(key string) string {
			var v string
			for _, line := range strings.Split(string(out), "\n") {
				if s, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
					v = strings.TrimSpace(s)
				}
			}
			return v
		}
		fail.replay, fail.debug = last("replay:"), last("debug:")
		log, _, _ := strings.Cut(last("log:"), " ")
		if fail.replay == "" || fail.debug == "" || log == "" {
			failErr = fmt.Errorf("detsched found no failure in testdata/simcompose:\n%s", out)
			return
		}
		data, err := os.ReadFile(log)
		if err != nil {
			failErr = err
			return
		}
		fail.starts = map[string]bool{}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "detsched: trace=") && fail.trace == "" {
				fail.trace = line // the run's; later ones are detsched's checks
			}
			// The server's starts, when serve runs on node server.
			if m := regexp.MustCompile(`^sim: ([0-9.]+) server: start$`).FindStringSubmatch(line); m != nil {
				fail.starts[m[1]] = true
			}
		}
		if fail.trace == "" || len(fail.starts) == 0 {
			failErr = fmt.Errorf("%s has no trace line or no server start", log)
		}
	})
	if failErr != nil {
		t.Fatal(failErr)
	}
	return &fail
}

// dlvPath returns the dlv to test.
func dlvPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("DLV"); p != "" {
		return p
	}
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(strings.TrimSpace(string(out)), "bin", "dlv")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("no dlv to test: %v (build it with misc/release/delve.bash bin/dlv, or set $DLV)", err)
	}
	return p
}

// runDlv runs the debug line of f with script as its --init file and
// flags, with the program's output in files, and returns Delve's output.
func runDlv(t *testing.T, f *failure, script string, flags ...string) string {
	t.Helper()
	dlv := dlvPath(t)
	dir := t.TempDir()
	init := filepath.Join(dir, "script.dlv")
	if err := os.WriteFile(init, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	line := strings.Replace(f.debug, " dlv exec ", " "+dlv+" exec ", 1)
	line += fmt.Sprintf(" -r stdout:%s/prog.out -r stderr:%s/prog.err --init %s --allow-non-terminal-interactive=true %s </dev/null",
		dir, dir, init, strings.Join(flags, " "))
	start := time.Now()
	out, err := exec.Command("/bin/sh", "-c", line).CombinedOutput()
	t.Logf("dlv ran %v with flags %q", time.Since(start).Round(time.Millisecond), flags)
	if err != nil {
		t.Fatalf("%s: %v\n%s", line, err, out)
	}
	return string(out)
}

// record is a line of dlv --json.
type record map[string]any

func (r record) str(key string) string {
	s, _ := r[key].(string)
	return s
}

func (r record) pos() record {
	p, _ := r["pos"].(map[string]any)
	return p
}

func (r record) num(key string) uint64 {
	f, _ := r[key].(float64)
	return uint64(f)
}

// parseJSON parses every line of dlv --json's output, which must all be
// records, since the program's output went to files.
func parseJSON(t *testing.T, out string) []record {
	t.Helper()
	var recs []record
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("dlv --json printed a line that is not JSON: %q: %v\n%s", sc.Text(), err, out)
		}
		recs = append(recs, r)
	}
	return recs
}

// The position on each tracepoint hit: serve runs on node server at the
// virtual time of each start of the server, the runtime's stop line at
// the first hit's entry tick names the same time, switch and goroutine,
// and the trace line at the end is the replay's.
func TestTracePositions(t *testing.T) {
	f := findFailure(t)
	out := runDlv(t, f, "trace serve simcompose.serve\ncontinue\nexit\n")
	re := regexp.MustCompile(`(?m)^> goroutine\((\d+)\): \[serve\] .*simcompose\.serve\(\) at t([0-9.]+) s(\d+) k(\d+) node server \(start \d+\)$`)
	hits := re.FindAllStringSubmatch(out, -1)
	if len(hits) == 0 {
		t.Fatalf("no tracepoint line with a position on node server:\n%s", out)
	}
	for _, h := range hits {
		if !f.starts[h[2]] {
			t.Errorf("serve ran at t%s, which is not a start of the server in the log", h[2])
		}
	}
	if !strings.Contains(out, "has exited with status 1 ("+f.trace+")") {
		t.Errorf("the exit does not have the replay's trace line %q:\n%s", f.trace, out)
	}

	// The runtime's stop line at the first hit's entry tick, the tick
	// after the one the breakpoint, before it, reports.
	k, _ := strconv.ParseUint(hits[0][4], 10, 64)
	want := fmt.Sprintf("at t%s s%s k%d goroutine %s", hits[0][2], hits[0][3], k+1, hits[0][1])
	if got := runtimeStop(t, f, fmt.Sprintf("k%d", k+1)); got != want {
		t.Errorf("the runtime stops %q with GODETSTOP=k%d, want %q", got, k+1, want)
	}
}

var stopRegexp = regexp.MustCompile(`(?m)^detsched: stop 1 (at t\S+ s\d+ k\d+ goroutine \d+)$`)

// runtimeStop runs the replay of f with GODETSTOP=point and returns the
// position of the runtime's stop line, "at tD sN kN goroutine G".
func runtimeStop(t *testing.T, f *failure, point string) string {
	t.Helper()
	replay := strings.Replace(f.replay, " GODETSEED=", " GODETSTOP="+point+" GODETSEED=", 1)
	out, _ := exec.Command("/bin/sh", "-c", replay).CombinedOutput()
	m := stopRegexp.FindSubmatch(out)
	if m == nil {
		t.Fatalf("the replay with GODETSTOP=%s printed no stop line:\n%s", point, out)
	}
	return string(m[1])
}

// A breakpoint stop, exit after it, and a script that ends there all
// leave the trace line, unchanged.
func TestExitTraceLine(t *testing.T) {
	f := findFailure(t)
	for _, script := range []string{
		"break s simcompose.serve\ncontinue\nprint detsched.time\nexit\n",
		"break s simcompose.serve\ncontinue\ncontinue\n",
	} {
		out := runDlv(t, f, script)
		if !regexp.MustCompile(`(?m)^> \[s\] .*simcompose\.serve\(\) .* at t[0-9.]+ s\d+ k\d+ node server \(start \d+\)$`).MatchString(out) {
			t.Errorf("no breakpoint stop with a position:\n%s", out)
		}
		if !strings.Contains(out, "has exited with status 1 ("+f.trace+")") {
			t.Errorf("script %q: no exit with the replay's trace line %q:\n%s", script, f.trace, out)
		}
	}
}

// Checkpoints, restart to them, also after the program exited, and to a
// position, rewind, goroutines by node, all in the JSON mode.
func TestCheckpointJSON(t *testing.T) {
	f := findFailure(t)
	// A switch halfway through the run, from its trace line.
	m := regexp.MustCompile(` switches=(\d+) `).FindStringSubmatch(f.trace)
	if m == nil {
		t.Fatalf("no switch count in the trace line %q", f.trace)
	}
	switches, _ := strconv.Atoi(m[1])
	point := fmt.Sprintf("s%d", switches/2)
	script := `break s simcompose.serve
continue
continue
checkpoint second
print detsched.tick
goroutines -with node server
continue
restart c1
print detsched.tick
rewind
print detsched.tick
clear s
continue
restart c1
print detsched.tick
restart ` + point + `
exit
`
	out := runDlv(t, f, script, "--json")
	defer func() {
		if t.Failed() {
			t.Logf("the replay: %s\ndlv's output:\n%s", f.replay, out)
		}
	}()
	recs := parseJSON(t, out)
	byCmd := map[string][]record{}
	for _, r := range recs {
		byCmd[r.str("cmd")] = append(byCmd[r.str("cmd")], r)
		if r.str("event") == "error" {
			t.Errorf("error: %v", r)
		}
	}
	var stops []record
	for _, r := range recs {
		if r.str("event") == "break" && r.str("bp") == "s" {
			stops = append(stops, r)
		}
	}
	// The third continue stops at serve again only in a run with three
	// starts of the server; it ends the run in others.
	if len(stops) < 2 {
		t.Fatalf("got %d stops at serve, want at least 2:\n%v", len(stops), recs)
	}
	for _, s := range stops {
		if !strings.HasPrefix(s.pos().str("node"), "server (start ") || s.pos().str("t") == "" {
			t.Errorf("stop without a position on node server: %v", s)
		}
	}
	second := strconv.FormatUint(stops[1].pos().num("k"), 10)
	values := byCmd["print detsched.tick"]
	if len(values) != 4 {
		t.Fatalf("got %d records for print detsched.tick, want 4: %v", len(values), values)
	}
	if v := values[0].str("value"); v != second {
		t.Errorf("detsched.tick at the second stop is %s, the stop's position says k%s", v, second)
	}
	if v := values[1].str("value"); v != second {
		t.Errorf("restart c1 went to k%s, want the checkpoint's k%s", v, second)
	}
	if v, want := values[2].str("value"), strconv.FormatUint(stops[0].pos().num("k"), 10); v != want {
		t.Errorf("rewind from the checkpoint went to k%s, want the first stop's k%s", v, want)
	}
	if v := values[3].str("value"); v != second {
		t.Errorf("restart c1 after the program exited went to k%s, want the checkpoint's k%s", v, second)
	}
	// restart sN stops where the runtime's GODETSTOP=sN does.
	var at string
	for _, r := range byCmd["restart "+point] {
		if r.str("event") == "stop" {
			p := r.pos()
			at = fmt.Sprintf("at t%s s%d k%d goroutine %d", p.str("t"), p.num("s"), p.num("k"), r.num("goroutine"))
		}
	}
	if want := runtimeStop(t, f, point); at != want {
		t.Errorf("restart %s stopped %q, the runtime's GODETSTOP=%s %q", point, at, point, want)
	}
	gs := byCmd["goroutines -with node server"]
	if len(gs) != 1 || gs[0].str("event") != "goroutines" {
		t.Fatalf("goroutines -with node server printed %v", gs)
	}
	list, _ := gs[0]["goroutines"].([]any)
	if len(list) == 0 {
		t.Errorf("no goroutine on node server")
	}
	for _, g := range list {
		if node, _ := g.(map[string]any)["node"].(string); !strings.HasPrefix(node, "server (start ") {
			t.Errorf("goroutines -with node server listed %v", g)
		}
	}
	exits := byCmd["exit"]
	if len(exits) == 0 || exits[len(exits)-1].str("event") != "exit" || exits[len(exits)-1].str("trace") != f.trace {
		t.Errorf("exit printed %v, want an exit record with the trace line %q", exits, f.trace)
	}
}

// restart to a position at the last tick of its goroutine's quantum, after
// which the goroutine yields, stops where GODETSTOP does: it stays in the
// runtime and names the goroutine's frame, as leaving the runtime for the
// goroutine's code would reach it only at a later switch. With quantum=1
// every tick is the last of a quantum.
func TestRestartWhereGoroutineYields(t *testing.T) {
	f := *findFailure(t)
	search := regexp.MustCompile(`GODETSEARCH='[^']*'`)
	f.replay = search.ReplaceAllString(f.replay, "GODETSEARCH='perturb=off quantum=1'")
	f.debug = search.ReplaceAllString(f.debug, "GODETSEARCH='perturb=off quantum=1'")
	run, _ := exec.Command("/bin/sh", "-c", f.replay).CombinedOutput()
	m := regexp.MustCompile(`(?m)^detsched: trace=\S+ switches=(\d+) `).FindSubmatch(run)
	if m == nil {
		t.Fatalf("the run with quantum=1 printed no trace line:\n%s", run)
	}
	switches, _ := strconv.Atoi(string(m[1]))
	s := fmt.Sprintf("s%d", switches/2)
	want := map[string]string{s: runtimeStop(t, &f, s)}
	k := "k" + regexp.MustCompile(` k(\d+) `).FindStringSubmatch(want[s])[1]
	want[k] = runtimeStop(t, &f, k)

	out := runDlv(t, &f, "restart "+s+"\nrestart "+k+"\nexit -k\n", "--json")
	defer func() {
		if t.Failed() {
			t.Logf("the debug line: %s\ndlv's output:\n%s", f.debug, out)
		}
	}()
	byCmd := map[string][]record{}
	for _, r := range parseJSON(t, out) {
		byCmd[r.str("cmd")] = append(byCmd[r.str("cmd")], r)
	}
	for _, point := range []string{s, k} {
		var at, fn, note string
		for _, r := range byCmd["restart "+point] {
			switch r.str("event") {
			case "stop":
				p := r.pos()
				at = fmt.Sprintf("at t%s s%d k%d goroutine %d", p.str("t"), p.num("s"), p.num("k"), r.num("goroutine"))
				fn = r.str("func")
			case "output":
				note = r.str("text")
			}
		}
		if at != want[point] {
			t.Errorf("restart %s stopped %q, the runtime's GODETSTOP=%s %q", point, at, point, want[point])
		}
		if fn != "runtime.detDebugStop" || !strings.Contains(note, " yields here") || !strings.Contains(note, "frame ") {
			t.Errorf("restart %s stopped in %s with the note %q, want runtime.detDebugStop and a note naming the goroutine's frame", point, fn, note)
		}
	}
}

// A hit count on a hot function, counted by the runtime, stops at the
// same hit as stock Delve's counting, which a condition forces, and the
// trace line stays the replay's in both.
func TestRuntimeCountedHits(t *testing.T) {
	f := findFailure(t)
	const n = 300
	ticks := map[bool]string{}
	took := map[bool]time.Duration{}
	for _, counted := range []bool{true, false} {
		script := fmt.Sprintf("break n time.Now\ncond -hitcount n == %d\n", n)
		if !counted {
			script += "cond n 1 == 1\n"
		}
		script += "breakpoints\ncontinue\nprint detsched.tick\nexit\n"
		start := time.Now()
		recs := parseJSON(t, runDlv(t, f, script, "--json"))
		took[counted] = time.Since(start)
		var hits, ok bool
		for _, r := range recs {
			switch r.str("cmd") {
			case "breakpoints":
				// Who counts the hits, and why not the runtime.
				bps, _ := r["breakpoints"].([]any)
				for _, b := range bps {
					if b := record(b.(map[string]any)); b.str("name") == "n" {
						ok = counted && b.str("counted_by") == "runtime" ||
							!counted && b.str("counted_by") == "delve" && b.str("not_runtime_counted") == "it has a condition besides the hit count"
					}
				}
			case "continue":
				if r.str("event") == "break" {
					hits = r.num("hits") == n
				}
			case "print detsched.tick":
				ticks[counted] = r.str("value")
			case "exit":
				if r.str("event") == "exit" && r.str("trace") != f.trace {
					t.Errorf("counted=%v: the trace line is %q, want %q", counted, r.str("trace"), f.trace)
				}
			}
		}
		if !ok {
			t.Errorf("counted=%v: breakpoints does not say whether the runtime counts:\n%v", counted, recs)
		}
		if !hits {
			t.Errorf("counted=%v: no stop at hit %d:\n%v", counted, n, recs)
		}
	}
	if ticks[true] == "" || ticks[true] != ticks[false] {
		t.Errorf("the runtime-counted breakpoint stopped at k%s, stock counting at k%s", ticks[true], ticks[false])
	}
	t.Logf("time.Now == %d on %s/%s: %v counted by the runtime, %v by Delve", n, runtime.GOOS, runtime.GOARCH, took[true].Round(time.Millisecond), took[false].Round(time.Millisecond))
}

// A tracepoint on a method promoted through an embedded struct, inlined
// into the compiler's wrappers, names the method in each hit, with the
// wrapper; and the returns of conditional tracepoints follow their
// entries, across goroutines and recursive calls. promoted is a plain
// program: this is Delve's, not detsched's.
func TestInlinedAndReturns(t *testing.T) {
	dlv := dlvPath(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "promoted")
	if out, err := exec.Command("go", "build", "-o", bin, "./promoted").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	script := filepath.Join(dir, "script.dlv")
	if err := os.WriteFile(script, []byte(`trace a main.(*base).Record
trace w main.work
cond w i == 3
trace d main.depth
cond d n == 2
continue
exit
`), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(flags ...string) string {
		args := append([]string{"exec", bin, "-r", "stdout:" + filepath.Join(dir, "prog.out"), "--init", script, "--allow-non-terminal-interactive=true"}, flags...)
		out, err := exec.Command(dlv, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("dlv %q: %v\n%s", args, err, out)
		}
		return string(out)
	}

	out := run()
	inl := regexp.MustCompile(`(?m)^> goroutine\(\d+\): \[a\] main\.\(\*base\)\.Record\(.*, \d\) \(inlined in main\.\(\*(list|index)\)\.Record\)$`)
	if n := len(inl.FindAllString(out, -1)); n != 4 {
		t.Errorf("got %d hit lines naming (*base).Record and its wrapper, want 4:\n%s", n, out)
	}
	for _, f := range []struct{ name, entry string }{{"work", `\[w\] main\.work\(3\)`}, {"depth", `\[d\] main\.depth\(2\)`}} {
		entries := regexp.MustCompile(`(?m)^> goroutine\((\d+)\): `+f.entry+`$`).FindAllStringSubmatch(out, -1)
		returns := regexp.MustCompile(`(?m)^>> goroutine\((\d+)\): \[`+f.name[:1]+`\] main\.`+f.name+` => `).FindAllStringSubmatch(out, -1)
		if len(entries) != 1 || len(returns) != 1 || entries[0][1] != returns[0][1] {
			t.Errorf("%s: got entries %q and returns %q, want one of each on the same goroutine:\n%s", f.name, entries, returns, out)
		}
	}

	var inlined, rets int
	for _, r := range parseJSON(t, run("--json")) {
		switch r.str("event") {
		case "trace":
			if r.str("bp") == "a" && (r.str("func") != "main.(*base).Record" || !strings.HasSuffix(r.str("inlined_in"), ").Record")) {
				t.Errorf("JSON hit of a: %v", r)
			} else if r.str("bp") == "a" {
				inlined++
			}
		case "return":
			// The tracepoint's name, as on its entries.
			if bp := r.str("bp"); bp != "w" && bp != "d" || !strings.HasSuffix(r.str("func"), "."+map[string]string{"w": "work", "d": "depth"}[bp]) {
				t.Errorf("JSON return record: %v", r)
			}
			rets++
		}
	}
	if inlined != 4 || rets != 2 {
		t.Errorf("dlv --json: %d hits of a with inlined_in, want 4; %d returns, want 2", inlined, rets)
	}
}

func TestLocations(t *testing.T) {
	dlv := dlvPath(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "app")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "replaced/app"
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	run := func(script string, flags ...string) string {
		path := filepath.Join(dir, "script.dlv")
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		args := append([]string{"exec", bin, "-r", "stdout:" + filepath.Join(dir, "prog.out"), "--init", path, "--allow-non-terminal-interactive=true"}, flags...)
		out, err := exec.Command(dlv, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("dlv %q: %v\n%s", args, err, out)
		}
		return string(out)
	}

	// Put's first line by the replaced module's import path; the closure
	// in a closure, in a module path with dots; Free, into which release
	// is inlined at Free's first statement, and then release, which Drop
	// also calls.
	script := `trace p github.com/acme/widgets/widgets.go:13
trace c github.com/acme/widgets.Visit.func1.1
trace f widgets.(*Set).Free
trace r widgets.(*Set).release
continue
exit
`
	out := run(script)
	for _, want := range []string{
		`(?m)^Tracepoint p set at .* for github\.com/acme/widgets\.Put\(\) `,
		`(?m)^Tracepoint c set at .* for github\.com/acme/widgets\.Visit\.func1\.1\(\) `,
		`(?m)^\ttracepoint r does not stop at 0x[0-9a-f]+, which tracepoint f has: `,
	} {
		if !regexp.MustCompile(want).MatchString(out) {
			t.Errorf("no match for %s:\n%s", want, out)
		}
	}
	for _, c := range []struct {
		re string
		n  int
	}{
		{`\[p\] github\.com/acme/widgets\.Put\(\d\)`, 3},
		{`\[c\] github\.com/acme/widgets\.Visit\.func1\.1\(\d\)`, 3},
		{`\[f\] github\.com/acme/widgets\.\(\*Set\)\.Free\(.*\) \(inlined in main\.main\)`, 2},
		{`\[r\] github\.com/acme/widgets\.\(\*Set\)\.release\(.*\) \(inlined in main\.main\)`, 1},
	} {
		if n := len(regexp.MustCompile(`(?m)^> goroutine\(1\): `+c.re+`$`).FindAllString(out, -1)); n != c.n {
			t.Errorf("got %d lines matching %s, want %d:\n%s", n, c.re, c.n, out)
		}
	}

	hits := map[string]int{}
	for _, r := range parseJSON(t, run(script, "--json")) {
		if e := r.str("event"); e == "trace" || e == "return" {
			hits[e+" "+r.str("bp")+" "+r.str("func")]++
		}
	}
	const widgets = "github.com/acme/widgets."
	want := map[string]int{
		"trace p " + widgets + "Put":            3,
		"trace c " + widgets + "Visit.func1.1":  3,
		"return c " + widgets + "Visit.func1.1": 3,
		"trace f " + widgets + "(*Set).Free":    2,
		"trace r " + widgets + "(*Set).release": 1,
	}
	if fmt.Sprint(hits) != fmt.Sprint(want) {
		t.Errorf("dlv --json: got hits %v, want %v", hits, want)
	}

	// goroutines -t: each goroutine's frames; and frames have one shape,
	// in a breakpoint's record (on b stack) and the stack command's.
	var gs []any
	frames := map[string]string{}
	for _, r := range parseJSON(t, run("break b widgets.Put\non b stack 2\ncontinue\nstack 2\ngoroutines -t 3\nexit -k\n", "--json")) {
		switch e := r.str("event"); e {
		case "goroutines":
			gs, _ = r["goroutines"].([]any)
		case "break", "stack":
			b, _ := json.Marshal(r["frames"])
			frames[e] = string(b)
			if _, ok := r["stack"]; ok {
				t.Errorf("a %s record with a stack field: %v", e, r)
			}
		}
	}
	if !strings.Contains(frames["break"], `"func":"`+widgets+`Put"`) || frames["break"] != frames["stack"] {
		t.Errorf("frames of the break record %s, of the stack record %s; want the same, from Put", frames["break"], frames["stack"])
	}
	found := false
	for _, g := range gs {
		fs, _ := g.(map[string]any)["frames"].([]any)
		if len(fs) == 0 {
			t.Errorf("goroutine without frames: %v", g)
			continue
		}
		if f, _ := fs[min(1, len(fs)-1)].(map[string]any); f["func"] == widgets+"Visit.func1.1" {
			found = true
		}
	}
	if !found {
		t.Errorf("goroutines -t 3 --json: no goroutine called Put from Visit.func1.1: %v", gs)
	}
}

// The results at a tracepoint's returns, which optimized code's debug
// information does not locate and Delve reads from the registers of the
// ABI, are the ones a build without optimizations shows, and the ones
// the source returns; and print and on ... print show a string of 300
// bytes whole. returns is a plain program.
func TestReturnValues(t *testing.T) {
	dlv := dlvPath(t)
	dir := t.TempDir()
	run := func(bin, script string) []record {
		path := filepath.Join(dir, "script.dlv")
		if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
			t.Fatal(err)
		}
		args := []string{"exec", bin, "-r", "stdout:" + filepath.Join(dir, "prog.out"), "--init", path, "--allow-non-terminal-interactive=true", "--json"}
		out, err := exec.Command(dlv, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("dlv %q: %v\n%s", args, err, out)
		}
		return parseJSON(t, string(out))
	}
	want := []string{
		`l "k1-value" | error nil`,
		`l "" | error(*errors.errorString) *{s: "empty key"}`,
		`c 21 | error nil`,
		`f *main.node {id: 4} | true`,
		`m main.pair {small: 5, big: 5497558138880}`,
		`r 0.75`,
		`s [2]int [9,10]`,
	}
	for _, gcflags := range []string{"", "all=-N -l"} {
		bin := filepath.Join(dir, "returns")
		if out, err := exec.Command("go", "build", "-gcflags="+gcflags, "-o", bin, "./returns").CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
		var got []string
		for _, r := range run(bin, "trace l main.lookup\ntrace c main.count\ntrace f main.find\ntrace m main.mix\ntrace r main.ratio\ntrace s main.span\ncontinue\nexit\n") {
			if r.str("event") != "return" {
				continue
			}
			var vals []string
			ret, _ := r["ret"].([]any)
			for _, v := range ret {
				vals = append(vals, record(v.(map[string]any)).str("value"))
			}
			got = append(got, r.str("bp")+" "+strings.Join(vals, " | "))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("-gcflags=%q: returns\n%s\nwant\n%s", gcflags, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if gcflags != "" {
			continue
		}
		banner := strconv.Quote(strings.Repeat("0123456789", 30))
		n := 0
		for _, r := range run(bin, "break l main.lookup\non l print main.banner\ncontinue\nprint main.banner\nexit -k\n") {
			v := r.str("value")
			if vars, _ := r["vars"].([]any); len(vars) == 1 {
				v = record(vars[0].(map[string]any)).str("value")
			}
			if strings.HasPrefix(v, `"0123`) {
				n++
				if v != banner {
					t.Errorf("%s printed a string of %d bytes, want %d", r.str("cmd"), len(v)-2, len(banner)-2)
				}
			}
		}
		if n != 2 {
			t.Errorf("got %d prints of main.banner, want 2", n)
		}
	}
}

// detsched.node is the node's name, without the start the position names,
// and detsched.start that start: a condition on both stops serve only in
// the second start of the server (the first in a run with one).
func TestNodeAndStart(t *testing.T) {
	f := findFailure(t)
	last := min(2, len(f.starts))
	script := fmt.Sprintf("trace s simcompose.serve\ncond s detsched.node == \"server\" && detsched.start == %d\non s print detsched.node\non s print detsched.start\ncontinue\nexit\n", last)
	var hits []record
	for _, r := range parseJSON(t, runDlv(t, f, script, "--json")) {
		if r.str("event") == "trace" {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits of serve with detsched.start == %d, want 1: %v", len(hits), last, hits)
	}
	vars, _ := hits[0]["vars"].([]any)
	var got []string
	for _, v := range vars {
		got = append(got, record(v.(map[string]any)).str("value"))
	}
	if want := []string{`"server"`, strconv.Itoa(last)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("detsched.node and detsched.start are %q, want %q", got, want)
	}
	if node, want := hits[0].pos().str("node"), fmt.Sprintf("server (start %d)", last); node != want {
		t.Errorf("the hit's position names node %q, want %q", node, want)
	}
}

// A channel prints in one line, with its length, capacity, whether it is
// closed and, in text or in the JSON record's addr, the address of the
// runtime's hchan, which *ch prints; and an on
// ... print is labelled with its expression, also for a field read
// through an interface. values is a plain program.
func TestValues(t *testing.T) {
	dlv := dlvPath(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "values")
	if out, err := exec.Command("go", "build", "-o", bin, "./values").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	script := filepath.Join(dir, "script.dlv")
	if err := os.WriteFile(script, []byte(`break w main.work
on w print j.env.err
on w print j.env.tries
on w print ch
continue
print ch
print *ch
exit
`), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"exec", bin, "-r", "stdout:" + filepath.Join(dir, "prog.out"), "--init", script, "--allow-non-terminal-interactive=true", "--json"}
	out, err := exec.Command(dlv, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("dlv %q: %v\n%s", args, err, out)
	}
	addr := regexp.MustCompile(`^0x[0-9a-f]+$`)
	var labels []string
	var prints int
	for _, r := range parseJSON(t, string(out)) {
		switch r.str("cmd") {
		case "continue":
			vars, _ := r["vars"].([]any)
			for _, v := range vars {
				v := record(v.(map[string]any))
				label := v.str("expr") + " = " + v.str("value")
				if addr.MatchString(v.str("addr")) {
					label += " at ADDR"
				}
				labels = append(labels, label)
			}
		case "print ch":
			prints++
			if r.str("value") != "chan int 3/100 (closed)" || !addr.MatchString(r.str("addr")) {
				t.Errorf("print ch: %v, want chan int 3/100 (closed) and the address", r)
			}
		case "print *ch":
			prints++
			if v := r.str("value"); !strings.Contains(v, "qcount: 3,") || !strings.Contains(v, "closed: 1,") {
				t.Errorf("print *ch: %q, want the hchan", v)
			}
		}
	}
	if prints != 2 {
		t.Errorf("got %d records of print ch and print *ch, want 2:\n%s", prints, out)
	}
	want := []string{`j.env.err = error(*errors.errorString) *{s: "boom"}`, `j.env.tries = 2`, `ch = chan int 3/100 (closed) at ADDR`}
	if fmt.Sprint(labels) != fmt.Sprint(want) {
		t.Errorf("on w print printed %q, want %q", labels, want)
	}

	// In text too, where structs print in several lines.
	if err := os.WriteFile(script, []byte("break w main.work\non w print ch\ncontinue\nprint ch\nexit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(dlv, args[:len(args)-1]...).CombinedOutput()
	if err != nil {
		t.Fatalf("dlv %q: %v\n%s", args, err, out)
	}
	if n := len(regexp.MustCompile(`(?m)^\t?(ch: )?chan int 3/100 \(closed\) at 0x[0-9a-f]+$`).FindAll(out, -1)); n != 2 || strings.Contains(string(out), "qcount") {
		t.Errorf("text: got %d one-line channels, want 2, and no hchan:\n%s", n, out)
	}
}
