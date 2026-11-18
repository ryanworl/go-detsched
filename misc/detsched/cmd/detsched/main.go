// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Detsched looks for schedules in which a package's tests fail, and prints
// a command that replays each failure.
//
// Usage:
//
//	detsched [-run regexp] [-duration d] [-n seeds] [-seed-base n] [-timeout d] [-p n] [-exec binary | package]
//
// Detsched builds the tests of the package (by default, the one in the
// current directory) with GOEXPERIMENT=detsched, or the package itself if
// it is a command without tests, and runs the binary once per seed
// (GODETSEED=1, 2, ...), as many at once as there are cores but one (-p
// sets how many), until it is interrupted, -duration passes, or -n seeds
// have run. Each seed runs another schedule, and with package
// detsched/sdk/sim, other faults. It groups the failing runs by their
// first assertion failure, panic, or fatal error, else their first failing
// test, keeps the output of the first few runs of each group, and prints a
// command that replays the group's first run: the same binary, directory,
// arguments, and seed, with GODETBRANCH naming the branch of a
// detsched/sdk/compose timeline that failed. It runs that command to check
// that the failure reproduces. To debug the failure, run the command with
// dlv exec in front of the binary and -- before its arguments.
//
// To spread a search over several machines, give each its own -seed-base:
// fresh seeds after seed 1 then start from it, so with -seed-base i<<32,
// shard i runs seeds i<<32+2, i<<32+3, and so on. To build the binary only
// once for them, build it with GOEXPERIMENT=detsched and CGO_ENABLED=0, as
// go test -c does, and pass it with -exec; detsched then runs it in the
// current directory instead of building a package.
//
// A run that takes longer than -timeout (default 10m) is stopped with
// SIGQUIT, which prints its goroutines, and counts as a failure.
//
// Detsched exits with status 1 if any run failed.
package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	runFlag  = flag.String("run", "", "run only the tests matching `regexp`, as go test -run does")
	duration = flag.Duration("duration", 0, "stop after this much time (default: run until interrupted)")
	seeds    = flag.Uint64("n", 0, "stop after this many seeds (default: run until interrupted)")
	timeout  = flag.Duration("timeout", 10*time.Minute, "stop a run that takes longer than this, and count it as a failure")
	procs    = flag.Int("p", max(runtime.NumCPU()-1, 1), "run this many programs at once")
	seedBase = flag.Uint64("seed-base", 0, "add `n` to every seed after seed 1, so machines searching together run disjoint seeds")
	execFlag = flag.String("exec", "", "run this prebuilt `binary` in the current directory instead of building a package")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: detsched [flags] [-exec binary | package]\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nDocumentation: %s\n", docsPath())
	}
	flag.Parse()
	if flag.NArg() > 1 || flag.NArg() == 1 && *execFlag != "" {
		flag.Usage()
		os.Exit(2)
	}
	if *seedBase >= 1<<62 {
		fmt.Fprintf(os.Stderr, "detsched: -seed-base must be below 2^62\n")
		os.Exit(2)
	}
	pkg := "."
	if flag.NArg() == 1 {
		pkg = flag.Arg(0)
	}
	s, err := newSearch(pkg, *execFlag, *runFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detsched: %v\n", err)
		os.Exit(2)
	}
	s.seedBase = *seedBase
	s.timeout = *timeout
	s.workers = max(*procs, 1)
	// For measuring the search: run only fresh, cell, or directed
	// launches, or plain runs of fresh seeds without forking.
	if a := os.Getenv("DETSCHED_ONLY"); a != "" {
		s.only = slices.Index(armNames[:], a)
		s.noFork = a == "plain"
	}
	s.progress = os.Stderr
	s.live = isTerminal(os.Stderr)
	lowerPriority()

	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals.Done()
		stop() // a second interrupt kills detsched
	}()
	s.signals = signals
	ctx := signals
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	err = s.run(ctx, *seeds)
	s.mu.Lock()
	failed := len(s.groups) > 0
	s.mu.Unlock()
	if failed && s.debugDone != nil {
		// Debug lines are the next step; give them the build without
		// optimizations if it is nearly done.
		select {
		case <-s.debugDone:
		case <-signals.Done():
		case <-time.After(2 * time.Minute):
		}
	}
	s.summary(os.Stdout, true)
	s.writeSummary(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detsched: %v\n", err)
		os.Exit(2)
	}
	if len(s.groups) > 0 {
		os.Exit(1)
	}
	// No failure: keep only the summary, whose path the search printed
	// at its start, the logs it names, and the binary when the summary
	// has replay lines.
	keep := s.summaryReplays()
	s.mu.Lock()
	debug := s.debugBin != ""
	s.mu.Unlock()
	entries, _ := os.ReadDir(s.out)
	for _, e := range entries {
		if e.Name() != "summary" && !strings.HasSuffix(e.Name(), ".log") && !(keep && (filepath.Join(s.out, e.Name()) == s.bin || e.Name() == "debug" && debug)) {
			os.RemoveAll(filepath.Join(s.out, e.Name()))
		}
	}
}

// A search runs a test binary with one seed after another.
type search struct {
	pkg        string
	dir        string   // the package's directory, where runs start
	bin        string   // the binary
	args       []string // its arguments
	env        []string // variables every run adds to the environment
	vars       []string // variables of our environment the schedule depends on, which replay lines set
	baseOutput []byte   // the output of the base run, seed 1
	out        string   // directory for the binary and the logs
	timeout    time.Duration
	workers    int
	seedBase   uint64 // added to every seed after seed 1

	progress io.Writer       // where to report progress, or nil
	signals  context.Context // done at SIGINT or SIGTERM, or nil
	live     bool            // progress is a terminal, where a status line updates in place

	// Set up by prepare.
	setup       bool   // the program reports the end of its setup
	stop        uint64 // the switch right after setup, which setup roots stop at
	maxSwitches uint64 // maxswitches of forked runs
	novelty     string // what counts as a new feature
	seen        []byte // features recorded so far, shared with every fork
	seenFile    *os.File

	mu          sync.Mutex
	start       time.Time
	rng         *rand.Rand
	runs        int
	failed      int
	tooLong     int           // runs stopped for too many switches, not counted as failures
	tooLongRun  *result       // the first of them
	slowPass    time.Duration // the longest passing run
	slowPassRun *result       // that run
	slowFail    time.Duration // the longest failing run, timeouts aside
	firstFail   time.Duration // when the first failure was recorded, after the search started
	firstRun    int           // the runs recorded by then
	groups      []*group      // in order of first appearance
	byKey       map[string]*group
	kinds       map[string]*also // every kind of failure, first in its run or not, by key
	arms        [numArms]arm
	cells       []*cell      // the archive
	lru         []*cell      // cells with stopped programs, least recently used first
	pool        []*cell      // setup roots
	claimed     map[int]bool // new features of plain runs' cells not stopped yet
	lowMem      bool         // memory ran low, which has been reported
	baseTrace   string       // the trace line of the base run, seed 1
	unstable    string       // how seed 1 ran two schedules, or ""
	newShell    bool         // seed 1 runs another schedule in a new shell, for variables findVars did not find
	rechecked   sync.Once    // runs recheck
	built       bool         // whether the search built the binary (not -exec)
	debugBin    string       // a build without optimizations that ran seed 1's schedule, or ""
	debugNote   string       // why debug lines run the optimized binary, or ""
	debugDone   chan bool    // closed when buildDebug is done, or nil
	left        []string     // where seed 1 left the deterministic subset, a line each
	aslr        string       // the runtime's report that seed 1 could not run without ASLR, or ""
	only        int          // the only arm to run, for measuring, or -1
	noFork      bool         // run fresh seeds without forking, for measuring
	roots       map[int]bool // process groups of the fork servers
	running     map[*liveRun]bool
	outOfMem    int           // runs stopped because memory ran out, not counted as failures
	bigStopped  *result       // the one of them that held the most memory
	memWait     time.Duration // time waitMem kept runs from starting
	memWaiting  int           // workers waitMem keeps waiting
	memWaitFrom time.Time     // when they started to
	tooLongLog  string        // the log of tooLongRun
	tooLongStat string        // whether its replay reproduced it
	features    int           // new features recorded during the search
	canFork     bool
	forked      bool           // a fork succeeded
	refused     int            // forks the program refused
	refusals    map[uint64]int // forks the program refused, by reason
	forks       int            // forks that succeeded
	refusal     uint64         // why the search stopped forking, or 0
	skipped     int            // runs in which a branch of a composer timeline did not run
	skipWhy     map[uint64]int // those runs, by why
	cut         int            // runs whose output was cut
	passed      int            // runs that passed
	passFaults  map[string]int // faults that fired in passing runs, by assertion message
	yieldSites  []string
	predicted   []string                // deadlocks the lock order predicts
	directed    [2]int                  // runs directed at predicted deadlocks, and those that failed
	bigOutput   int64                   // the most output a run wrote
	asserts     map[string]*assertCount // always, sometimes and reachable assertions, by kind and message
}

// An assertCount counts the runs that reached an assertion, and the runs
// in which it held.
type assertCount struct {
	kind, msg  string
	runs, held int
}

// A group is the failing runs with one signature.
type group struct {
	key    string           // the signature with numbers masked
	sig    string           // the signature of the first run
	runs   int              // runs that failed this way
	logs   []string         // the kept logs
	replay string           // the command that replays the first run
	debug  string           // the same under Delve
	status string           // whether running replay reproduced the failure
	index  int              // its number, from 1, which its logs' names have
	also   map[string]*also // other failures in its runs, by key
	left   string           // for a deadlock at the end of the test, the goroutines left
	faults map[string]int   // faults that fired in its runs, by assertion message
}

// An also is a failure of another kind in a group's runs, such as a
// symptom of the failure the runs fail with first.
type also struct {
	sig    string // its signature in the first run that had it
	runs   int
	log    string // the log of that run
	replay string // the command that replays that run
}

// keepLogs is how many logs of each group a search keeps.
const keepLogs = 10

// newSearch builds pkg in a new directory, or if bin is set, runs that
// prebuilt binary in the current directory.
func newSearch(pkg, bin, run string) (*search, error) {
	out, err := os.MkdirTemp("", "detsched-")
	if err != nil {
		return nil, err
	}
	s := &search{
		pkg:     pkg,
		out:     out,
		timeout: 10 * time.Minute,
		workers: runtime.NumCPU(),
		byKey:   map[string]*group{},
		kinds:   map[string]*also{},
		roots:   map[int]bool{},
		running: map[*liveRun]bool{},
		claimed: map[int]bool{},
		asserts: map[string]*assertCount{},
		rng:     rand.New(rand.NewPCG(1, 2)),

		refusals:   map[uint64]int{},
		skipWhy:    map[uint64]int{},
		passFaults: map[string]int{},
		only:       -1,
	}
	if bin != "" {
		// Runs and replay lines use a copy, so that rebuilding the
		// binary during or after the search changes neither.
		s.pkg = bin
		s.dir, err = os.Getwd()
		if err == nil {
			s.bin = filepath.Join(out, filepath.Base(bin))
			err = copyFile(s.bin, bin)
		}
	} else {
		s.dir, s.bin, err = buildPackage(pkg, out)
		s.built = true
	}
	if err != nil {
		os.RemoveAll(out)
		return nil, err
	}
	if run != "" {
		s.args = []string{"-test.run=" + run}
	}
	return s, nil
}

// goCmd returns the go command: the one next to detsched, which is
// installed in the toolchain's bin directory, else the one on $PATH.
func goCmd() string {
	if exe, err := os.Executable(); err == nil {
		if g := filepath.Join(filepath.Dir(exe), "go"); isFile(g) {
			return g
		}
	}
	return "go"
}

// copyFile copies the executable src to dst.
func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

func isFile(name string) bool {
	fi, err := os.Stat(name)
	return err == nil && fi.Mode().IsRegular()
}

// buildPackage builds pkg's test binary, or pkg itself if it is a command
// without tests, with the experiment and without cgo, which a process
// that forks a branch of a timeline must not use. It returns the
// package's directory and the binary. flags go to the go command.
func buildPackage(pkg, out string, flags ...string) (dir, bin string, err error) {
	exp := "detsched"
	if e := os.Getenv("GOEXPERIMENT"); e != "" {
		exp = e + ",detsched"
	}
	if v, err := exec.Command(goCmd(), "env", "GOVERSION").Output(); err == nil && !strings.Contains(string(v), "detsched") {
		return "", "", fmt.Errorf("%s is %s, not a detsched toolchain; install a detsched release and run its detsched", goCmd(), strings.TrimSpace(string(v)))
	}
	env := append(os.Environ(), "GOEXPERIMENT="+exp, "CGO_ENABLED=0")
	list := exec.Command(goCmd(), "list", "-f", "{{.Dir}} {{.Name}} {{len .TestGoFiles}} {{len .XTestGoFiles}}", pkg)
	list.Env = env
	b, err := list.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			b = ee.Stderr
		}
		return "", "", fmt.Errorf("%s: %v\n%s", pkg, err, b)
	}
	f := strings.Fields(string(b))
	if len(f) != 4 {
		return "", "", fmt.Errorf("%s: want one package, got:\n%s", pkg, b)
	}
	dir = f[0]
	bin = filepath.Join(out, filepath.Base(dir)+".test")
	args := []string{"test", "-c", "-o", bin, pkg}
	if f[2] == "0" && f[3] == "0" {
		if f[1] != "main" {
			return "", "", fmt.Errorf("%s has no tests", pkg)
		}
		bin = filepath.Join(out, filepath.Base(dir))
		args = []string{"build", "-o", bin, pkg}
	}
	args = slices.Insert(args, 1, flags...)
	cmd := exec.Command(goCmd(), args...)
	cmd.Env = env
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("building %s: %v\n%s", pkg, err, b)
	}
	return dir, bin, nil
}

// A result is what one run did.
type result struct {
	seed        uint64   // GODETSEED
	tape        string   // the branch points from the seed, or ""
	yield       string   // the sites the run yields at, or ""
	env         []string // the search's variables when it ran
	arm         int
	output      []byte // standard output and error
	newFeatures int    // features no run had recorded before
	exitCode    int
	timedOut    bool
	hung        bool // stopped when the search ended, after mayHang said it may hang
	wall        time.Duration
	outOfMem    bool   // stopped because memory ran out
	memAtStop   uint64 // the private memory it held then, if known
	outputSize  int64  // the bytes of output it wrote, which may be more than output holds
	maxSwitches uint64 // the maxswitches it ran with, or 0
}

var (
	traceLine     = regexp.MustCompile(`(?m)^detsched: trace=0x[0-9a-f]+ switches=\d+ syncs=\d+$`)
	traceHash     = regexp.MustCompile(`(?m)^detsched: trace=(0x[0-9a-f]+) `)
	branchWrapper = regexp.MustCompile(`\(GODETBRANCH=(\d+) runs it in place\)( \(and \d+ more failures?\))?$`)
	assertCounts  = regexp.MustCompile(`(?m)^detsched: assert (always|always-or-unreachable|sometimes|reachable) true=(\d+) false=\d+ (.*)$`)
	switchesField = regexp.MustCompile(` switches=(\d+) `)
	envReadLine   = regexp.MustCompile(`(?m)^detsched: environment read: (.*)$`)
	syncsField    = regexp.MustCompile(` syncs=(\d+)$`)
	setupLine     = regexp.MustCompile(`(?m)^detsched: setup complete at switch (\d+)$`)
	deadlockLine  = regexp.MustCompile(`(?m)^detsched: potential deadlock: (.*)$`)
	directiveLine = regexp.MustCompile(`(?m)^detsched: deadlock directive yield=(\S+)$`)
	leftReport    = regexp.MustCompile(`(?m)^detsched: .*; the program no longer runs deterministically\n(?:.+\n)*`) // and its stack
	failureLine   = regexp.MustCompile(`(?m)^(detsched: assertion failed: |panic: |fatal error: ).*`)
	testFailure   = regexp.MustCompile(`(?m)^\s*--- FAIL: \S+`)
	branchLine    = regexp.MustCompile(`(?m)^detsched: branch (\d+) \(GODETBRANCH=\d+ runs it in place\)$`)
	skippedLine   = regexp.MustCompile(`(?m)^detsched: branch \d+ did not run: the process cannot fork now \((.*)\)$`)
	assertDetails = regexp.MustCompile(`^detsched: assertion failed: .*?( map\[| \(and \d+ more failures?\)$)`)
	errDetails    = regexp.MustCompile(`^map\[err:("(?:[^"\\]|\\.)*"|[^ "]*)\]( \(and \d+ more failures?\))?$`)
	masked        = regexp.MustCompile(`0x[0-9a-f]+|\b[0-9A-Fa-f]{16,}\b|\d+`)
)

func (r *result) failed() bool { return r.exitCode != 0 || r.timedOut }

// tooLong reports whether the run was stopped for making too many
// switches since its last branch, which is not a failure of the program:
// a run that loops in virtual time, or one forked from deep in a search.
func (r *result) tooLong() bool {
	return r.exitCode != 0 && bytes.Contains(r.output, []byte("more switches than maxswitches"))
}

// trace returns the run's first trace line, which is that of the
// branch whose output comes first, or "".
func (r *result) trace() string { return string(traceLine.Find(r.output)) }

// spun returns the branch of the composer timeline that a run stopped for
// too many switches stopped in, or 0 for the timeline, and the trace line
// printed where it stopped.
func (r *result) spun() (int, string) {
	i := bytes.Index(r.output, []byte("more switches than maxswitches"))
	if i < 0 {
		return r.branch(), r.trace()
	}
	rest := r.output[i:]
	loc := traceLine.FindIndex(rest)
	if loc == nil {
		return 0, ""
	}
	trace := string(rest[loc[0]:loc[1]])
	if m := branchLine.FindSubmatchIndex(rest[loc[1]:]); m != nil && m[0] == 1 {
		n, _ := strconv.Atoi(string(rest[loc[1]+m[2] : loc[1]+m[3]]))
		return n, trace
	}
	return 0, trace
}

// signature returns the line that tells how the run failed: its first
// assertion failure, panic, or fatal error (a branch of a composer
// timeline that fails writes its output first, unless the timeline had
// failed before it), else its first failing test, else "timeout", or
// for a run the search's end stopped, which may hang, "still running when
// the search ended", or its exit status.
func (r *result) signature() string {
	line := failureLine.Find(r.output)
	if line == nil {
		line = bytes.TrimSpace(testFailure.Find(r.output))
	}
	switch {
	case line != nil:
		return truncate(string(line), 600)
	case r.hung:
		return "still running when the search ended"
	case r.timedOut:
		return "timeout"
	}
	return fmt.Sprintf("exit status %d", r.exitCode)
}

// key returns the signature with the numbers in it masked, so that one
// bug makes one group.
// An assertion failure's key is its message, without its details or the
// count of other failures.
func (r *result) key() string { return failureKey(r.signature()) }

// failureKey returns the key of a signature.
func failureKey(sig string) string {
	msg, _ := splitSignature(sig)
	return masked.ReplaceAllString(msg, "N")
}

// splitSignature splits an assertion failure's signature into its message
// and its details, which are only the first failure's. When the details
// are only an error, as a compose check reports its failure, the message
// keeps the error's first line, so that a check that fails in different
// ways, such as a safety and a liveness violation, makes a group of each.
func splitSignature(sig string) (msg, details string) {
	m := assertDetails.FindStringSubmatchIndex(sig)
	if m == nil {
		return sig, ""
	}
	msg, details = sig[:m[2]], sig[m[2]+1:]
	if e := errDetails.FindStringSubmatch(details); e != nil {
		if u, uerr := strconv.Unquote(e[1]); uerr == nil {
			e[1] = u
		}
		err, _, _ := strings.Cut(e[1], "\n")
		msg += ": " + truncate(err, 160)
	}
	return msg, details
}

// failures returns the run's assertion failures, panics, and fatal
// errors, each once.
func (r *result) failures() []string {
	var lines []string
	for _, line := range failureLine.FindAll(r.output, -1) {
		if l := truncate(string(line), 600); !slices.Contains(lines, l) {
			lines = append(lines, l)
		}
	}
	return lines
}

// branch returns the number of the composer branch the failure happened
// in, or 0 for the timeline itself.
func (r *result) branch() int {
	// The branch whose output comes first, before the timeline's, failed
	// first: its trace line, the run's first, is followed by its number.
	// A branch that died before it printed one is named by the run's
	// first failure, the timeline's report of it.
	var m [][]byte
	if first := traceLine.FindIndex(r.output); first != nil {
		rest := r.output[first[1]:]
		if next := traceLine.FindIndex(rest); next != nil {
			rest = rest[:next[0]]
		}
		m = branchLine.FindSubmatch(rest)
	}
	if m == nil {
		m = branchWrapper.FindSubmatch([]byte(r.signature()))
	}
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func atou(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// addSearch adds fields to GODETSEARCH in env, the runtime's internal
// interface for a search (see runtime/detsched.go's detSearchInit).
func addSearch(env []string, fields ...string) []string {
	if len(fields) == 0 {
		return env
	}
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "GODETSEARCH="); ok {
			env = slices.Clone(env)
			env[i] = "GODETSEARCH=" + strings.Join(append([]string{v}, fields...), " ")
			return env
		}
	}
	return append(env, "GODETSEARCH="+strings.Join(fields, " "))
}

func (s *search) temp(name string) string { return filepath.Join(s.out, name) }

// replayLine returns a shell command line that runs r again, in place of
// the branch of a composer timeline it failed in, if any. With debug, it
// runs it under Delve instead: --disable-aslr because on macOS the
// program re-executes itself without ASLR, which loses Delve's
// breakpoints. A run stopped for too many switches replays with the same
// limit, so that it stops at the same switch; one stopped because memory
// ran out replays in a subshell that caps its memory, on Linux.
func (s *search) replayLine(r *result, debug bool) string {
	branch, trace := r.branch(), r.trace()
	if r.tooLong() {
		branch, trace = r.spun()
	}
	env := []string{fmt.Sprintf("GODETSEED=%d", r.seed), fmt.Sprintf("GODETBRANCH=%d", branch)}
	if r.tape != "" {
		env = addSearch(env, "tape="+r.tape)
	}
	env = addSearch(env, yieldField(r.yield)...)
	if r.tooLong() {
		env = addSearch(env, fmt.Sprintf("maxswitches=%d", r.maxSwitches))
	}
	// The schedule the line replays, which the run checks its own against,
	// unless the run was stopped from outside, where the replay goes on.
	if m := traceHash.FindStringSubmatch(trace); m != nil && !r.timedOut && !r.outOfMem {
		env = addSearch(env, "trace="+m[1])
	}
	env = slices.Concat(env, r.env, s.vars)
	// Set the variables the program read as the run had them, and unset
	// those it did not have, since the shell the line runs in may have
	// others, such as NO_COLOR, which can change the schedule.
	var unset []string
	for _, k := range s.envRead(r) {
		if slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, k+"=") }) {
			continue
		}
		if v, ok := lookupEnv(slices.Concat(os.Environ(), s.env, r.env), k); !ok {
			unset = append(unset, k)
		} else if !slices.Contains([]string{"HOME", "PATH", "USER", "LOGNAME", "SHELL", "PWD"}, k) {
			env = append(env, k+"="+v)
		}
	}
	var b strings.Builder
	capped := r.outOfMem && r.memAtStop > 0 && !debug && runtime.GOOS == "linux"
	if capped {
		// The address space of a run starts at about 1.3 GB.
		fmt.Fprintf(&b, "(ulimit -v %d; ", (r.memAtStop+r.memAtStop/4+3<<29)>>10)
	}
	fmt.Fprintf(&b, "cd %s && ", quote(s.dir))
	if len(unset) > 0 {
		b.WriteString("env")
		for _, k := range unset {
			b.WriteString(" -u " + quote(k))
		}
		b.WriteString(" ")
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		fmt.Fprintf(&b, "%s=%s ", k, quote(v))
	}
	if debug {
		b.WriteString("dlv exec --disable-aslr ")
	}
	b.WriteString(quote(s.bin))
	if debug && len(s.args) > 0 {
		b.WriteString(" --")
	}
	for _, a := range s.args {
		b.WriteString(" " + quote(a))
	}
	if capped {
		b.WriteString(")")
	}
	return b.String()
}

// debugLine returns g's debug line, which runs the build without
// optimizations once it has run seed 1's schedule, so that dlv reads
// every local. The caller holds s.mu.
func (s *search) debugLine(g *group) string {
	if s.debugBin == "" {
		return g.debug
	}
	return strings.Replace(g.debug, "dlv exec --disable-aslr "+quote(s.bin), "dlv exec --disable-aslr "+quote(s.debugBin), 1)
}

// buildDebug builds the program with -gcflags=all=-N -l, for debug lines,
// and runs seed 1 in it: with detsched's ticks, which count where the
// source has them, it runs the same schedule as the optimized build
// unless the program reads its own stacks or its work depends on heap
// addresses, and then debug lines keep the optimized binary.
func (s *search) buildDebug(ctx context.Context) {
	defer close(s.debugDone)
	dir := filepath.Join(s.out, "debug")
	err := os.Mkdir(dir, 0o777)
	var bin string
	if err == nil {
		_, bin, err = buildPackage(s.pkg, dir, "-gcflags=all=-N -l")
	}
	var r *result
	// Up to three runs: a run on a loaded machine can leave the
	// deterministic subset, such as when a system call outlasts the
	// watchdog, while a build that runs another schedule does so every
	// time.
	for try := 0; err == nil && try < 3 && (r == nil || r.trace() != s.baseTrace); try++ {
		cmd := s.plain(1, addSearch(nil, "feedback=1")...)
		cmd.Path, cmd.Args[0] = bin, bin
		r, err = s.exec(context.WithoutCancel(ctx), cmd)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errors.Is(err, errCanceled):
	case err != nil:
		s.debugNote = fmt.Sprintf("building or running it without optimizations failed: %v", truncate(err.Error(), 500))
		s.notice("debug lines run the optimized binary: building or running it without optimizations failed")
	case r.trace() != s.baseTrace:
		s.debugNote = fmt.Sprintf("its build without optimizations (-gcflags=all=-N -l) ran seed 1 on another schedule three times (%s, not %s), as a program that reads its own stacks or whose work depends on heap addresses does", strings.TrimPrefix(r.trace(), "detsched: "), strings.TrimPrefix(s.baseTrace, "detsched: "))
		s.notice("debug lines run the optimized binary: its build without optimizations runs another schedule")
	default:
		s.debugBin = bin
		s.printf("detsched: debug lines from here on run %s, built without optimizations (-gcflags=all=-N -l), which runs the same schedules and in which dlv reads every local\n", bin)
		for _, g := range s.groups {
			s.printf("\tgroup %d (%s): %s\n", g.index, g.msg(), s.debugLine(g))
		}
	}
}

// envRead returns the names of the variables the program read, in the
// base run or in r, which print them (GODETSEARCH's envread), but those
// that replay lines and debugging set themselves.
func (s *search) envRead(r *result) []string {
	var names []string
	for _, out := range [][]byte{s.baseOutput, r.output} {
		if m := envReadLine.FindSubmatch(out); m != nil {
			for _, k := range strings.Fields(string(m[1])) {
				if !strings.HasPrefix(k, "GODET") && k != "GODEBUG" && !slices.Contains(names, k) {
					names = append(names, k)
				}
			}
		}
	}
	return names
}

// lookupEnv returns the value of k in env, the last one if it is set more
// than once, as exec.Cmd does.
func lookupEnv(env []string, k string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], k+"="); ok {
			return v, true
		}
	}
	return "", false
}

// splitEnv splits our environment into the variables a new shell of the
// same user has too, and the rest, which a replay line may have to set.
// It leaves out the variables runs get from detsched.
func splitEnv() (shell, rest []string) {
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		switch {
		case k == "PWD" || k == "OLDPWD" || k == "GODEBUG" || strings.HasPrefix(k, "GODET"):
		case strings.HasPrefix(k, "LC_") || slices.Contains([]string{"HOME", "PATH", "USER", "LOGNAME", "SHELL", "TERM", "TMPDIR", "LANG"}, k):
			shell = append(shell, e)
		default:
			rest = append(rest, e)
		}
	}
	return shell, rest
}

// shellRun returns the base run, seed 1, in a new shell's environment
// with vars added.
func (s *search) shellRun(vars []string) *exec.Cmd {
	shell, _ := splitEnv()
	cmd := exec.Command(s.bin, s.args...)
	cmd.Dir = s.dir
	cmd.Env = slices.Concat(shell, vars, s.env, []string{"PWD=" + s.dir, "GODETSEED=1"})
	return cmd
}

// findVars returns the variables of our environment that the command
// run(vars) needs, in a new shell's environment with vars added, to run
// the schedule with trace line trace: besides the ones a new shell has
// and s.vars, a variable such as SIMCOMPOSE_BUG=1 that a test reads to
// plant a bug. It leaves out groups of variables in parallel, splitting
// each group whose absence changes the schedule, and reports whether the
// variables it found are enough.
func (s *search) findVars(ctx context.Context, run func(vars []string) *exec.Cmd, trace string) ([]string, bool, error) {
	_, rest := splitEnv()
	s.mu.Lock()
	rest = slices.DeleteFunc(rest, func(v string) bool { return slices.Contains(s.vars, v) })
	s.mu.Unlock()
	split := func(vars []string) [][]string {
		var parts [][]string
		for n := min(len(vars), max(s.workers, 2)); n > 0; n-- {
			k := len(vars) / n
			parts = append(parts, vars[:k])
			vars = vars[k:]
		}
		return parts
	}
	var need []string
	sem := make(chan bool, s.workers)
	for groups := split(rest); len(groups) > 0; {
		changed := make([]bool, len(groups))
		errs := make([]error, len(groups))
		var wg sync.WaitGroup
		for i, g := range groups {
			wg.Add(1)
			sem <- true
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				var r *result
				r, errs[i] = s.exec(ctx, run(slices.DeleteFunc(slices.Clone(rest), func(v string) bool { return slices.Contains(g, v) })))
				changed[i] = errs[i] == nil && r.trace() != trace
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return nil, false, err
		}
		var next [][]string
		for i, g := range groups {
			switch {
			case !changed[i]:
			case len(g) == 1:
				need = append(need, g[0])
			default:
				next = append(next, split(g)...)
			}
		}
		groups = next
	}
	r, err := s.exec(ctx, run(need))
	if err != nil {
		return nil, false, err
	}
	return need, r.trace() == trace, nil
}

var shellSafe = regexp.MustCompile(`^[\w@%+=:,./-]+$`)

func quote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// maxOutput is the most of a run's output detsched keeps: its first and
// last halves, with a line where the rest was, so that a run that writes
// gigabytes does not run the search out of memory. The failure lines and
// the fault timeline come at the end of the output.
const maxOutput = 16 << 20

// A cappedOutput keeps the first and the last maxOutput/2 bytes written to
// it.
type cappedOutput struct {
	head, tail []byte // tail is a ring once full, starting at at
	at         int
	n          int64 // bytes written
}

func (c *cappedOutput) Write(p []byte) (int, error) {
	n := len(p)
	c.n += int64(n)
	k := min(len(p), maxOutput/2-len(c.head))
	c.head = append(c.head, p[:k]...)
	for p = p[k:]; len(p) > 0; {
		if len(c.tail) < maxOutput/2 {
			k := min(len(p), maxOutput/2-len(c.tail))
			c.tail = append(c.tail, p[:k]...)
			p = p[k:]
			continue
		}
		k := copy(c.tail[c.at:], p)
		c.at = (c.at + k) % len(c.tail)
		p = p[k:]
	}
	return n, nil
}

func (c *cappedOutput) bytes() []byte {
	if c.n <= maxOutput {
		return append(c.head, c.tail...)
	}
	cut := fmt.Sprintf("\n[detsched: %d bytes of the run's output cut here]\n", c.n-maxOutput)
	return slices.Concat(c.head, []byte(cut), c.tail[c.at:], c.tail[:c.at])
}

// readOutput reads a run's output from the file named name, as a
// cappedOutput keeps it, and returns it and its size.
func readOutput(name string) ([]byte, int64) {
	f, err := os.Open(name)
	if err != nil {
		return nil, 0
	}
	defer f.Close()
	var c cappedOutput
	io.Copy(&c, f)
	return c.bytes(), c.n
}

// errCanceled reports a run stopped because the search ended.
var errCanceled = errors.New("canceled")

// exec runs cmd in its own process group, which holds the branches it
// forks, and stops the group when the run takes longer than s.timeout or
// ctx is done.
func (s *search) exec(ctx context.Context, cmd *exec.Cmd) (*result, error) {
	var out cappedOutput
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second // a branch left behind may hold the output open
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	defer syscall.Kill(-pgid, syscall.SIGKILL)
	untrack := s.track(start, pgid, func(sig syscall.Signal) { syscall.Kill(-pgid, sig) })
	defer untrack()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	r := &result{env: s.env}
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		if !s.mayHang(start) {
			syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
			return nil, errCanceled
		}
		r.timedOut, r.hung = true, true
	case <-timer.C:
		r.timedOut = true
	}
	if r.timedOut {
		// SIGQUIT, for the goroutines' stacks.
		syscall.Kill(-pgid, syscall.SIGQUIT)
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			syscall.Kill(-pgid, syscall.SIGKILL)
			err = <-done
		}
	}
	r.wall = time.Since(start)
	r.outOfMem, r.memAtStop = untrack()
	r.output, r.outputSize = out.bytes(), out.n
	if ee, ok := err.(*exec.ExitError); ok {
		r.exitCode = ee.ExitCode()
	} else if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return nil, err
	}
	return r, nil
}

// run runs the base run, then launches until ctx is done or, if n is not
// 0, n runs have run.
func (s *search) run(ctx context.Context, n uint64) error {
	s.start = time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.progress != nil {
		stop := s.report()
		defer stop()
	}
	defer s.closeAll()
	go s.guardMem(ctx)
	if err := s.prepare(ctx); err != nil {
		if errors.Is(err, errCanceled) {
			return nil
		}
		return err
	}
	// Check seed 1's schedule again a minute in, as well as at the end,
	// so that a search stopped by a signal checks it too.
	recheck := time.AfterFunc(time.Minute, func() { s.rechecked.Do(s.recheck) })
	defer recheck.Stop()
	jobs := make(chan uint64)
	go func() {
		defer close(jobs)
		for i := uint64(2); n == 0 || i <= n; i++ {
			select {
			case jobs <- s.seedBase + i:
			case <-ctx.Done():
				return
			}
		}
		s.mu.Lock()
		s.printf("detsched: the last of the %d runs started; waiting for the runs still going\n", n)
		s.mu.Unlock()
	}()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for range max(s.workers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := range jobs {
				s.waitMem(ctx)
				err := s.launch(ctx, seed)
				if errors.Is(err, errCanceled) {
					return
				}
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					cancel()
					return
				}
			}
		}()
	}
	wg.Wait()
	// Stopped by a signal, the search ends at once, before what the
	// sender may kill it for (as a tool's stop often follows SIGTERM
	// with SIGKILL a second later).
	if firstErr == nil && !errors.Is(ctx.Err(), context.Canceled) {
		s.rechecked.Do(func() {
			s.mu.Lock()
			s.printf("detsched: %d runs done; running seed 1 once more, to check that its schedule has not changed, before the summary\n", s.runs)
			s.mu.Unlock()
			s.recheck()
		})
	}
	return firstErr
}

// recheck runs seed 1 once more, a minute or more after the base run in
// real time, and warns if it runs another schedule: a program that reads
// the real clock outside a bubble, such as at init, often runs the same
// schedule in two runs started at once. A signal, also after -duration
// ended the search, stops it and goes on to the summary.
func (s *search) recheck() {
	ctx := s.signals
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := s.exec(ctx, s.plain(1))
	if err != nil || r.trace() == s.baseTrace {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unstable == "" {
		s.unstable = fmt.Sprintf("seed 1 ran two different schedules: %q at the start of the search, %q %v later", strings.TrimPrefix(s.baseTrace, "detsched: "), strings.TrimPrefix(r.trace(), "detsched: "), time.Since(s.start).Round(time.Second))
		s.notice(s.unstable)
	}
}

// record records a run, and when it is the first of its group, replays it.
func (s *search) record(ctx context.Context, r *result, a int) error {
	r.arm = a
	if r.env == nil {
		r.env = s.env
	}
	s.mu.Lock()
	s.predict(r.output)
	s.countAsserts(r.output)
	s.countSkipped(r.output)
	s.bigOutput = max(s.bigOutput, r.outputSize)
	if r.outputSize > maxOutput {
		s.cut++
	}
	s.runs++
	if a == armDirected {
		s.directed[0]++
		if r.failed() && !r.tooLong() && !r.outOfMem {
			s.directed[1]++
		}
	}
	if r.tooLong() {
		s.tooLong++
		if s.tooLongRun != nil {
			s.mu.Unlock()
			return nil
		}
		s.tooLongRun, s.tooLongLog, s.tooLongStat = r, s.temp("spin.log"), "replaying"
		line := s.replayLine(r, false)
		s.mu.Unlock()
		if err := os.WriteFile(s.tooLongLog, append([]byte("# "+line+"\n"), r.output...), 0o666); err != nil {
			return err
		}
		status, err := s.replay(ctx, line, r)
		if errors.Is(err, errCanceled) {
			status = "was not run: the search ended first"
		} else if err != nil {
			return err
		}
		s.mu.Lock()
		s.tooLongStat = status
		s.mu.Unlock()
		return nil
	}
	if r.outOfMem {
		s.outOfMem++
		if s.bigStopped != nil && r.memAtStop <= s.bigStopped.memAtStop {
			s.mu.Unlock()
			return nil
		}
		s.bigStopped = r
		line := s.replayLine(r, false)
		s.mu.Unlock()
		return os.WriteFile(s.temp("memory.log"), append([]byte("# "+line+"\n"), r.output...), 0o666)
	}
	faults := faultsOf(r.output)
	if !r.failed() {
		s.passed++
		for _, f := range faults {
			s.passFaults[f]++
		}
		if r.wall > s.slowPass {
			s.slowPass, s.slowPassRun = r.wall, r
		}
		// A run that makes 1000 times the switches of a whole run that
		// passed spins; stopCell passes the limit to forked runs.
		if m := switchesField.FindStringSubmatch(r.trace()); m != nil && s.maxSwitches == 0 {
			s.maxSwitches = 1000 * max(atou(m[1]), 1000)
		}
		s.mu.Unlock()
		return nil
	}
	s.failed++
	if s.failed == 1 {
		s.firstFail, s.firstRun = time.Since(s.start), s.runs
	}
	if !r.timedOut {
		s.slowFail = max(s.slowFail, r.wall)
	}
	g := s.byKey[r.key()]
	first := g == nil
	if first {
		g = &group{key: r.key(), sig: r.signature(), replay: s.replayLine(r, false), debug: s.replayLine(r, true), status: "replaying", index: len(s.groups) + 1, also: map[string]*also{}, faults: map[string]int{}}
		if strings.Contains(g.sig, "blocked goroutines remain") {
			g.left = leftGoroutines(r.output)
		}
		s.byKey[g.key] = g
		s.groups = append(s.groups, g)
	}
	g.runs++
	for _, f := range faults {
		g.faults[f]++
	}
	counted := map[string]bool{g.key: true}
	if s.kinds[g.key] == nil {
		s.kinds[g.key] = &also{sig: g.sig}
	}
	s.kinds[g.key].runs++
	line := s.replayLine(r, false)
	var news []*also  // kinds no run of the group had before
	var fresh []*also // kinds no run had before
	for _, f := range r.failures() {
		k := failureKey(f)
		if counted[k] || branchWrapper.MatchString(f) {
			continue // the timeline's report of its failed branch, whose failure comes first
		}
		counted[k] = true
		if s.kinds[k] == nil {
			s.kinds[k] = &also{sig: f}
			fresh = append(fresh, s.kinds[k])
		}
		s.kinds[k].runs++
		if g.also[k] == nil {
			g.also[k] = &also{sig: f, replay: line}
			news = append(news, g.also[k])
		}
		g.also[k].runs++
	}
	// The group keeps its first runs' logs, and the log of the first run
	// of each other kind of failure.
	var log string
	if len(g.logs) < keepLogs || len(news) > 0 {
		log = s.temp(fmt.Sprintf("fail-%d-%d.log", g.index, len(g.logs)+1))
		g.logs = append(g.logs, log)
	}
	for _, a := range news {
		a.log = log
	}
	// Another kind of failure, after the group's, may be another bug,
	// which its run's replay line shows. The group's first run prints
	// its other kinds under the group's own lines, once replayed.
	var firstAlso string
	for _, a := range fresh {
		if first {
			firstAlso += "\talso:   " + a.sig + "\n"
			continue
		}
		s.printf("ALSO %s\n\tafter:  %s\n\tlog:    %s\n\treplay: %s\n", a.sig, g.msg(), log, line)
	}
	s.mu.Unlock()

	writeLog := func(line string) error {
		if log == "" {
			return nil
		}
		// The failures head the log, which may be long.
		head := "# " + line + "\n"
		for _, f := range r.failures() {
			head += "# " + f + "\n"
		}
		return os.WriteFile(log, append([]byte(head), r.output...), 0o666)
	}
	if err := writeLog(line); err != nil {
		return err
	}
	if !first {
		return nil
	}
	status, err := s.replay(ctx, g.replay, r)
	if err == nil && status != "reproduced" {
		// The run may depend on a variable that seed 1 does not read.
		line := g.replay
		vars, ok, ferr := s.findVars(ctx, func(vars []string) *exec.Cmd {
			cmd := exec.Command("sh", "-c", line)
			shell, _ := splitEnv()
			cmd.Env = append(shell, vars...)
			return cmd
		}, r.trace())
		if err = ferr; err == nil && ok && len(vars) > 0 {
			s.mu.Lock()
			for _, v := range vars {
				if !slices.Contains(s.vars, v) {
					s.vars = append(s.vars, v)
				}
			}
			g.replay, g.debug = s.replayLine(r, false), s.replayLine(r, true)
			s.mu.Unlock()
			if err := writeLog(g.replay); err != nil {
				return err
			}
			status, err = s.replay(ctx, g.replay, r)
		}
	}
	if errors.Is(err, errCanceled) {
		status = "was not run: the search ended first"
	} else if err != nil {
		return err
	}
	s.mu.Lock()
	g.status = status
	s.printf("FAIL %s\n%s\tlog:    %s\n\treplay: %s\n\tdebug:  %s\n\t%s\n", g.sig, firstAlso, g.logs[0], g.replay, s.debugLine(g), replayVerdict(status))
	s.mu.Unlock()
	return nil
}

// replayVerdict returns the line under a group that says whether its
// replay reproduced it, which warns when it did not.
func replayVerdict(status string) string {
	switch status {
	case "reproduced":
		return "the replay reproduced the failure in a new shell"
	case "replaying":
		return "the replay is being checked"
	}
	return "WARNING: the replay " + status
}

// countSkipped counts a run in which a branch of a composer timeline did
// not run because the process could not fork. s.mu must be held.
func (s *search) countSkipped(out []byte) {
	if m := skippedLine.FindSubmatch(out); m != nil {
		s.skipped++
		s.skipWhy[branchReasons[string(m[1])]]++
	}
}

// faultsOf returns the faults that fired in a run, as the simulation
// reports them: the assertions "fault: SITE" and "nemesis: KIND" that
// were reached.
func faultsOf(out []byte) []string {
	var fs []string
	for _, m := range assertCounts.FindAllSubmatch(out, -1) {
		msg := string(m[3])
		if string(m[1]) == "reachable" && string(m[2]) != "0" && (strings.HasPrefix(msg, "fault: ") || strings.HasPrefix(msg, "nemesis: ")) && !slices.Contains(fs, msg) {
			fs = append(fs, msg)
		}
	}
	return fs
}

// replay runs the replay command line with a shell, as a user would in
// a new shell, and reports whether it failed as r did.
func (s *search) replay(ctx context.Context, line string, r *result) (string, error) {
	cmd := exec.Command("sh", "-c", line)
	cmd.Env, _ = splitEnv()
	again, err := s.exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	want, got := r.trace(), again.trace()
	if r.tooLong() {
		_, want = r.spun()
		_, got = again.spun()
	}
	switch {
	case again.key() != r.key():
		return fmt.Sprintf("did not reproduce the failure: it ended with %q", again.signature()), nil
	case got != want && !r.timedOut: // a run stopped from outside stops anywhere
		return fmt.Sprintf("failed the same way, but ran another schedule (%q, not %q)", got, want), nil
	}
	return "reproduced", nil
}

// printf prints a line above the status line. s.mu must be held.
func (s *search) printf(format string, args ...any) {
	if s.progress == nil {
		return
	}
	if s.live {
		fmt.Fprint(s.progress, "\r\033[K")
	}
	fmt.Fprintf(s.progress, format, args...)
	if s.live {
		fmt.Fprint(s.progress, s.status())
	}
}

// hangLimit returns how long a run goes on before the search counts it
// as one that may hang: 10 times the slowest pass, and at least 10 s, and
// why, to print after it. s.mu must be held.
func (s *search) hangLimit() (time.Duration, string) {
	if limit := 10 * s.slowPass; limit >= 10*time.Second {
		return limit, " (10 times the slowest pass)"
	}
	return 10 * time.Second, ""
}

// mayHang reports whether a run that started at start has gone on past
// hangLimit. When the search ends, such a run is stopped with SIGQUIT and
// recorded, with its stacks and replay line, rather than dropped.
func (s *search) mayHang(start time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit, _ := s.hangLimit()
	return time.Since(start) > limit
}

// status returns a line of counts, including the runs going on for over
// 10 times as long as any run that passed, which may hang, so that they
// show before -timeout stops them. s.mu must be held.
func (s *search) status() string {
	d := time.Since(s.start)
	line := fmt.Sprintf("%d runs, %d failed, %d signatures, %d features, %.1f runs/s, %v", s.runs, s.failed, len(s.groups), s.features, float64(s.runs)/d.Seconds(), d.Round(time.Second))
	if s.slowPass >= 5*time.Second {
		line += fmt.Sprintf("; slowest passing run %v", s.slowPass.Round(time.Second))
	}
	limit, why := s.hangLimit()
	slow, oldest := 0, time.Duration(0)
	for l := range s.running {
		if age := time.Since(l.start); !l.stopped && age > limit {
			slow++
			oldest = max(oldest, age)
		}
	}
	if slow > 0 {
		line += fmt.Sprintf("; %d runs going on for over %v%s, the oldest for %v; -timeout is %v", slow, limit.Round(time.Second), why, oldest.Round(time.Second), s.timeout)
	}
	if s.memWaiting > 0 {
		line += "; waiting for memory to start runs"
	}
	return line
}

// report prints the status line every second on a terminal, else after
// 5 seconds and then every 10, and writes the summary so far to a file
// every 30 seconds, until the returned function is called.
func (s *search) report() (stop func()) {
	every := 10 * time.Second
	if s.live {
		every = time.Second
	}
	fmt.Fprintf(s.progress, "detsched: the summary so far is in %s, every 30 seconds\n", s.temp("summary"))
	t := time.NewTicker(min(every, 5*time.Second))
	done := make(chan bool)
	go func() {
		last := time.Now()
		for first := true; ; first = false {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if first {
				t.Reset(every)
			}
			s.mu.Lock()
			if s.live {
				fmt.Fprint(s.progress, "\r\033[K"+s.status())
			} else {
				fmt.Fprintln(s.progress, "detsched: "+s.status())
			}
			s.mu.Unlock()
			if time.Since(last) >= 30*time.Second {
				last = time.Now()
				s.writeSummary(false)
			}
		}
	}()
	return func() {
		t.Stop()
		close(done)
		if s.live {
			fmt.Fprint(s.progress, "\r\033[K")
		}
	}
}

// writeSummary writes the summary to the file "summary" in the search's
// directory, replacing it whole: the summary so far, or with final, the
// summary of the search that ended.
func (s *search) writeSummary(final bool) {
	var b bytes.Buffer
	s.summary(&b, final)
	if os.WriteFile(s.temp("summary.new"), b.Bytes(), 0o666) == nil {
		os.Rename(s.temp("summary.new"), s.temp("summary"))
	}
}

// docsPath returns where detsched's README is: in the toolchain whose go
// command detsched runs, else its path in a release.
func docsPath() string {
	if g, err := exec.LookPath(goCmd()); err == nil {
		if p := filepath.Join(filepath.Dir(g), "..", "misc", "detsched", "README.md"); isFile(p) {
			return filepath.Clean(p)
		}
	}
	return "misc/detsched/README.md"
}

// A fallback is something that limited the search, or what its replay or
// debug lines can do, which the user can fix.
type fallback struct {
	anchor string // its section of the README, under Fallbacks
	what   string // what happened
	cost   string // what it costs
	fix    string // how to fix it
}

// fallbacks returns the search's fallbacks. s.mu must be held.
func (s *search) fallbacks() []fallback {
	var fs []fallback
	add := func(anchor, what, cost, fix string) { fs = append(fs, fallback{anchor, what, cost, fix}) }
	if s.unstable != "" {
		add("seed-1-ran-two-schedules", s.unstable,
			"failures may not replay: the program depends on something besides its seed",
			"find what it reads outside a synctest bubble, usually time.Now() at package init: diff two runs of seed 1 with GODEBUG=dettrace=2")
	}
	if len(s.left) > 0 {
		add("seed-1-left-the-deterministic-subset", fmt.Sprintf("seed 1 left the deterministic subset in %d places, first: %s", len(s.left), s.left[0]),
			"failures may not replay",
			"move that work into the synctest bubble or onto a sim node; each place's stack is in "+s.temp("seed-1.log"))
	}
	if s.aslr != "" {
		add("aslr", "seed 1 could not run itself again without ASLR: "+s.aslr,
			"schedules may differ between runs, and failures may not replay",
			"run the binary as a regular program, not set-user-ID, from a writable directory")
	}
	if s.newShell {
		add("new-shell", "seed 1 runs another schedule in a new shell, and detsched could not find which of its variables cause it",
			"replay lines may not reproduce failures in another shell",
			"run replay lines from the shell that ran detsched")
	}
	if r, ok := forkReasons[s.refusal]; ok {
		add("fork-refused", "the program refused to fork: "+r.why,
			"the search ran fresh seeds only, so it starts fewer runs per second and finds bugs that need several rare events in a row later",
			r.fix)
	} else if s.refused > 0 {
		var most uint64
		var why []string
		for _, k := range slices.Sorted(maps.Keys(s.refusals)) {
			why = append(why, fmt.Sprintf("%d %s", s.refusals[k], strings.TrimPrefix(errRefused{k}.Error(), "fork refused: ")))
			if s.refusals[k] > s.refusals[most] {
				most = k
			}
		}
		add("fork-refused", fmt.Sprintf("%d of %d forks were refused: %s", s.refused, s.refused+s.forks, strings.Join(why, "; ")),
			"those launches ran a fresh seed from the start instead",
			forkReasons[most].fix)
	}
	if c := s.asserts["sometimes "+setupAssert]; c != nil && c.held == 0 {
		add("no-faults", "the simulation injected no disk or network faults: no run's World finished setting up",
			"no run tested failed fsyncs, bad sectors, a full disk, or late or reset packets",
			"call lifecycle.SetupComplete(nil) when setup is done, or start a Nemesis, or drive the test with compose")
	}
	if s.skipped > 0 {
		var most uint64
		for k, n := range s.skipWhy {
			if n > s.skipWhy[most] {
				most = k
			}
		}
		add("branch-did-not-run", fmt.Sprintf("in %d of %d runs a branch of the composer timeline did not run: the process could not fork (%s)", s.skipped, s.runs, forkReasons[most].why),
			"those branches' Eventually and Finally checks did not run",
			forkReasons[most].fix)
	}
	var bad, unchecked []string
	for _, g := range s.groups {
		switch {
		case g.status == "was not run: the search ended first":
			unchecked = append(unchecked, strconv.Itoa(g.index))
		case g.status != "reproduced" && g.status != "replaying":
			bad = append(bad, strconv.Itoa(g.index))
		}
	}
	if len(bad) > 0 {
		add("replay-did-not-reproduce", fmt.Sprintf("the replays of %d failures did not reproduce them in a new shell: groups %s", len(bad), strings.Join(bad, ", ")),
			"their replay and debug lines may show another run than the failure",
			"read the WARNING under each; diff GODEBUG=dettrace=2 runs of the replay line to find where runs part")
	}
	if len(unchecked) > 0 {
		add("replay-not-checked", fmt.Sprintf("the replays of %d failures were not checked, as the search ended first: groups %s", len(unchecked), strings.Join(unchecked, ", ")),
			"their replay lines may not reproduce them",
			"run a replay line and check that its trace line equals the first one in the log")
	}
	// With -exec, the user built the binary, and the debug line runs it.
	if s.debugBin == "" && s.built && (s.debugNote != "" || len(s.groups) > 0) {
		why, fix := s.debugNote, "debug the optimized binary: read values at function entries, where arguments are readable"
		if why == "" {
			why = "the build without optimizations was not ready when the search ended"
			fix = "build the same source with -gcflags='all=-N -l' and put it in place of the binary on the debug line (see delve.md)"
		}
		add("debug-lines-run-the-optimized-binary", "debug lines run the optimized binary: "+why,
			"dlv cannot read some locals or step line by line", fix)
	}
	if s.outOfMem > 0 || s.lowMem {
		what := "memory ran low"
		if s.memWait >= time.Second {
			what += fmt.Sprintf("; the search waited %v to start runs", s.memWait.Round(time.Second))
		}
		fix := "bound what the harness keeps, such as its history and logs, or lower -p"
		if s.outOfMem > 0 {
			what += fmt.Sprintf("; it stopped %d runs", s.outOfMem)
			if r := s.bigStopped; r.memAtStop > 0 {
				what += fmt.Sprintf("; the largest held %d MB, whose stacks are in %s", r.memAtStop>>20, s.temp("memory.log"))
				fix += "; replay the largest, with its memory capped on Linux: " + s.replayLine(r, false)
			}
		}
		add("memory", what, "fewer runs, and stopped runs count as neither pass nor failure, so the search lost what they would have found", fix)
	}
	if s.tooLong > 0 {
		add("switch-limit", fmt.Sprintf("%d runs made over 1000 times the switches of a passing run and were stopped, not counted as failures; the first one's log is %s, and its replay %s", s.tooLong, s.tooLongLog, strings.TrimPrefix(replayVerdict(s.tooLongStat), "the replay ")),
			"a run that loops forever in virtual time is a liveness bug that the search did not count",
			"read its log's stacks, or replay it, which stops at the same switch: "+s.replayLine(s.tooLongRun, false))
	}
	if g := s.byKey[failureKey("still running when the search ended")]; g != nil {
		limit, why := s.hangLimit()
		add("still-running", fmt.Sprintf("%d runs were still running after %v%s when the search ended: group %d", g.runs, limit.Round(time.Second), why, g.index),
			"they may hang, or only be slow",
			"read the stacks in the group's logs; a replay that ends was only slow")
	}
	if s.cut > 0 {
		add("output-cut", fmt.Sprintf("%d runs wrote more than %d MB of output; their logs keep the first and last %d MB", s.cut, maxOutput>>20, maxOutput>>21),
			"the middle of their output is lost",
			"print diagnostics once per run, after an assertion failed (assert.Failures() > 0)")
	}
	return fs
}

// setupAssert is the message of the assertion package sim makes as its
// World closes, true if the World finished setting up.
const setupAssert = "sim: setup completed, so the simulation injected faults"

// notice prints a fallback as it happens. s.mu must be held.
func (s *search) notice(what string) {
	s.printf("detsched: fallback: %s (the summary says what it costs and how to fix it)\n", what)
}

// summary prints the counts, the fallbacks, the search's statistics, and
// every group. Before the search ends, final is false, and the summary
// says it is not the last.
func (s *search) summary(w io.Writer, final bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !final {
		fmt.Fprintf(w, "detsched: the search is still running; this file is rewritten every 30 seconds until it ends\n")
	}
	fmt.Fprintf(w, "detsched: %s: %s\n", s.pkg, s.status())
	if fs := s.fallbacks(); len(fs) > 0 {
		docs := docsPath()
		what := fmt.Sprintf("%d fallbacks", len(fs))
		if len(fs) == 1 {
			what = "1 fallback"
		}
		fmt.Fprintf(w, "\nWARNING: %s limited this search; each costs what it says until fixed:\n", what)
		for _, f := range fs {
			fmt.Fprintf(w, "  - %s\n    cost: %s\n    fix:  %s\n    docs: %s#%s\n", f.what, f.cost, f.fix, docs, f.anchor)
		}
		fmt.Fprintln(w)
	}
	if s.runs == 0 {
		return
	}
	var mix []string
	for i, a := range s.arms {
		if a.launches > 0 {
			mix = append(mix, fmt.Sprintf("%d %s", a.launches, armNames[i]))
		}
	}
	if len(mix) > 0 {
		fmt.Fprintf(w, "detsched: runs started: %s; %d cells kept; features count %s\n", strings.Join(mix, ", "), len(s.cells), s.novelty)
	}
	if len(s.vars) > 0 {
		fmt.Fprintf(w, "detsched: replay lines set these variables of detsched's environment, which the schedule depends on: %s\n", strings.Join(s.vars, " "))
	}
	if len(s.predicted) > 0 && s.directed[1] == 0 {
		fmt.Fprintf(w, "detsched: %d lock orders predicted a deadlock, and none of the %d runs directed at them failed, so they are likely false alarms (a freed lock's memory reused for another)\n", len(s.predicted), s.directed[0])
	} else if len(s.predicted) > 0 {
		fmt.Fprintf(w, "detsched: lock orders that predict a deadlock, at which %d runs were directed, %d of which failed (a prediction no directed run fails is likely a false alarm: a freed lock's memory reused for another):\n", s.directed[0], s.directed[1])
		for _, p := range s.predicted {
			fmt.Fprintf(w, "\t%s\n", p)
		}
	}
	if s.failed > 0 {
		fmt.Fprintf(w, "detsched: first failure after %v, at run %d\n", s.firstFail.Round(100*time.Millisecond), s.firstRun)
	}
	if s.slowFail > 0 {
		fmt.Fprintf(w, "detsched: slowest passing run %v, slowest failing run %v; -timeout is %v\n", s.slowPass.Round(time.Millisecond), s.slowFail.Round(time.Millisecond), s.timeout)
	} else {
		fmt.Fprintf(w, "detsched: slowest passing run %v; -timeout is %v\n", s.slowPass.Round(time.Millisecond), s.timeout)
	}
	if r := s.slowPassRun; r != nil && s.slowPass >= 5*time.Second {
		fmt.Fprintf(w, "detsched: the slowest passing run, to replay when runs are slower than the program's work explains:\n\treplay: %s\n", s.replayLine(r, false))
	}
	s.assertSummary(w)
	s.kindSummary(w)
	if len(s.groups) > 0 && !slices.ContainsFunc(s.groups, func(g *group) bool { return g.status != "reproduced" }) {
		fmt.Fprintf(w, "detsched: every failure's replay line reproduced it in a new shell\n")
	}
	for _, g := range s.groups {
		// The heading leaves out the details, which are only the first
		// run's.
		msg, details := splitSignature(g.sig)
		fmt.Fprintf(w, "\nFAIL %d of %d runs: %s\n", g.runs, s.runs, msg)
		if details != "" {
			fmt.Fprintf(w, "\tits first run: %s\n", details)
		}
		if g.left != "" {
			fmt.Fprintf(w, "\tthe goroutines left in the bubble, by where they wait and what created them: %s\n", g.left)
		}
		if f := s.faultLine(g); f != "" {
			fmt.Fprintf(w, "\tfaults in its runs: %s\n", f)
		}
		more := ""
		if len(g.logs) > 1 {
			more = fmt.Sprintf(" (and %d more of these runs: %s)", len(g.logs)-1, filepath.Join(s.out, fmt.Sprintf("fail-%d-*.log", g.index)))
		}
		fmt.Fprintf(w, "\tlog:    %s%s\n", g.logs[0], more)
		fmt.Fprintf(w, "\treplay: %s\n\tdebug:  %s\n", g.replay, s.debugLine(g))
		if g.status != "reproduced" {
			fmt.Fprintf(w, "\t%s\n", replayVerdict(g.status))
		}
		alsos := slices.SortedFunc(maps.Values(g.also), func(a, b *also) int { return cmp.Or(b.runs-a.runs, strings.Compare(a.sig, b.sig)) })
		for i, a := range alsos {
			if i == 10 {
				fmt.Fprintf(w, "\tand %d other kinds of failure, each first in a log above, whose first line replays it\n", len(alsos)-i)
				break
			}
			fmt.Fprintf(w, "\talso in %d of these runs: %s (first in %s)\n", a.runs, a.sig, filepath.Base(a.log))
			if a.replay != g.replay {
				fmt.Fprintf(w, "\t\tits replay: %s\n", a.replay)
			}
		}
	}
}

// faultLine describes the faults that fired in most of a group's runs, in
// at least 3, and in a share of them at least 25 points above the share
// of passing runs they fired in, most frequent first, as "SITE in N of M
// (P% of passing runs)". s.mu must be held.
func (s *search) faultLine(g *group) string {
	type corr struct {
		name       string
		n          int
		lift, pass float64
	}
	var cs []corr
	for f, n := range g.faults {
		share := float64(n) / float64(g.runs)
		pass := 0.0
		if s.passed > 0 {
			pass = float64(s.passFaults[f]) / float64(s.passed)
		}
		if n >= 3 && 2*n >= g.runs && share-pass >= 0.25 && !strings.HasPrefix(f, "nemesis: a ") && f != "nemesis: overlapping faults" {
			cs = append(cs, corr{strings.Replace(strings.TrimPrefix(f, "fault: "), "nemesis: ", "nemesis ", 1), n, share - pass, pass})
		}
	}
	slices.SortFunc(cs, func(a, b corr) int {
		return cmp.Or(b.n-a.n, cmp.Compare(b.lift, a.lift), strings.Compare(a.name, b.name))
	})
	var parts []string
	for i, c := range cs {
		if i == 3 {
			break
		}
		p := "no run passed"
		if s.passed > 0 {
			p = fmt.Sprintf("%.0f%% of passing runs", 100*c.pass)
		}
		parts = append(parts, fmt.Sprintf("%s in %d of %d (%s)", c.name, c.n, g.runs, p))
	}
	return strings.Join(parts, ", ")
}

// kindSummary prints how many runs failed each way, first or after
// another failure, when some run failed more than one way. s.mu must be
// held.
func (s *search) kindSummary(w io.Writer) {
	if !slices.ContainsFunc(s.groups, func(g *group) bool { return len(g.also) > 0 }) {
		return
	}
	ks := slices.SortedFunc(maps.Values(s.kinds), func(a, b *also) int { return cmp.Or(b.runs-a.runs, strings.Compare(a.sig, b.sig)) })
	fmt.Fprintf(w, "detsched: runs that failed each way, first or after another failure:\n")
	for i, k := range ks {
		if i == 20 {
			fmt.Fprintf(w, "\t(and %d more)\n", len(ks)-i)
			break
		}
		msg, _ := splitSignature(k.sig)
		fmt.Fprintf(w, "\t%d: %s\n", k.runs, msg)
	}
}

// countAsserts counts the always, sometimes and reachable assertions of a run's
// reports: the timeline's, at the end of its output, and those of the
// branches it forked that failed, which come first. An assertion is
// reached, or true, in the run if it was in any of them. s.mu must be
// held.
func (s *search) countAsserts(out []byte) {
	held := map[string]bool{}
	for _, m := range assertCounts.FindAllSubmatch(out, -1) {
		k := string(m[1]) + " " + string(m[3])
		held[k] = held[k] || string(m[2]) != "0"
	}
	for k, h := range held {
		c := s.asserts[k]
		if c == nil {
			kind, msg, _ := strings.Cut(k, " ")
			c = &assertCount{kind: kind, msg: msg}
			s.asserts[k] = c
		}
		c.runs++
		if h {
			c.held++
		}
	}
}

// sdkAsserts are the prefixes of the SDK's own assertions, which say what
// faults and composer features runs reached.
var sdkAsserts = []string{"fault: ", "nemesis: ", "compose: ", "node panicked: "}

// assertSummary prints the sometimes and reachable assertions, those never
// true first, then on one line how many runs evaluated each always
// assertion, and on one line those of the SDK. s.mu must be held.
func (s *search) assertSummary(w io.Writer) {
	cs := slices.SortedFunc(maps.Values(s.asserts), func(a, b *assertCount) int {
		return cmp.Or(cmp.Compare(min(a.held, 1), min(b.held, 1)), strings.Compare(a.kind, b.kind), strings.Compare(a.msg, b.msg))
	})
	var own, always []string
	sdk := make([][]string, len(sdkAsserts))
	for _, c := range cs {
		if c.msg == setupAssert {
			continue // a fallback when never true
		}
		i := slices.IndexFunc(sdkAsserts, func(p string) bool { return strings.HasPrefix(c.msg, p) })
		switch {
		case strings.HasPrefix(c.kind, "always"):
			always = append(always, fmt.Sprintf("%q %d", c.msg, c.runs))
		case i >= 0:
			if c.held > 0 {
				sdk[i] = append(sdk[i], fmt.Sprintf("%s %d", strings.TrimPrefix(c.msg, sdkAsserts[i]), c.held))
			}
		case c.kind == "reachable":
			own = append(own, fmt.Sprintf("reachable %q: reached in %d runs", c.msg, c.held))
		case c.held == 0:
			own = append(own, fmt.Sprintf("sometimes %q: NEVER TRUE, in the %d runs that reached it", c.msg, c.runs))
		default:
			own = append(own, fmt.Sprintf("sometimes %q: true in %d of the %d runs that reached it", c.msg, c.held, c.runs))
		}
	}
	if len(own) > 0 {
		fmt.Fprintf(w, "detsched: sometimes and reachable assertions (one no run reached is not listed; messages are cut at 120 bytes):\n")
		for i, l := range own {
			if i == 40 {
				fmt.Fprintf(w, "\t(and %d more)\n", len(own)-i)
				break
			}
			fmt.Fprintf(w, "\t%s\n", l)
		}
	}
	if len(always) > 0 {
		if len(always) > 40 {
			always = append(always[:40], fmt.Sprintf("and %d more", len(always)-40))
		}
		fmt.Fprintf(w, "detsched: runs that evaluated each always assertion (one no run evaluated is not listed): %s\n", strings.Join(always, ", "))
	}
	for i, l := range sdk {
		if len(l) > 0 {
			fmt.Fprintf(w, "detsched: runs that reached each %s assertion: %s\n", strings.TrimSuffix(sdkAsserts[i], ": "), strings.Join(l, ", "))
		}
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
}

// msg returns the group's signature without the first run's details.
func (g *group) msg() string {
	msg, _ := splitSignature(g.sig)
	return msg
}

// summaryReplays reports whether the summary prints a replay line of a
// run that did not fail, which names the kept binary.
func (s *search) summaryReplays() bool {
	return s.tooLong > 0 || s.outOfMem > 0 && s.bigStopped != nil && s.bigStopped.memAtStop > 0 || s.slowPassRun != nil && s.slowPass >= 5*time.Second
}

// goroutineHeader matches a goroutine's header in a traceback, with its
// state.
var goroutineHeader = regexp.MustCompile(`(?m)^goroutine \d+ \[([^\]]*)\]:\n`)

// leftGoroutines describes the goroutines of a synctest bubble in the
// output of a run whose test body returned while they were blocked: the
// function each waits in and the one that created it, most common first.
func leftGoroutines(out []byte) string {
	counts := map[string]int{}
	locs := goroutineHeader.FindAllSubmatchIndex(out, -1)
	for i, l := range locs {
		if !bytes.Contains(out[l[2]:l[3]], []byte("synctest bubble")) {
			continue
		}
		end := len(out)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := out[l[1]:end]
		if j := bytes.Index(block, []byte("\n\n")); j >= 0 {
			block = block[:j]
		}
		var top, creator string
		for _, line := range strings.Split(string(block), "\n") {
			switch {
			case strings.HasPrefix(line, "created by "):
				creator = strings.Fields(strings.TrimPrefix(line, "created by "))[0]
			case top == "" && line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "runtime.") && !strings.HasPrefix(line, "internal/") && !strings.HasPrefix(line, "sync.") && !strings.HasPrefix(line, "time."):
				top = line
				if k := strings.LastIndex(top, "("); k > 0 {
					top = top[:k]
				}
			}
		}
		if top == "" && creator == "" {
			continue
		}
		counts[top+" (created by "+creator+")"]++
	}
	keys := slices.SortedFunc(maps.Keys(counts), func(a, b string) int { return cmp.Or(counts[b]-counts[a], strings.Compare(a, b)) })
	for i, k := range keys {
		if counts[k] > 1 {
			keys[i] = fmt.Sprintf("%d × %s", counts[k], k)
		}
	}
	if len(keys) > 5 {
		keys = append(keys[:5], fmt.Sprintf("and %d more", len(keys)-5))
	}
	return strings.Join(keys, ", ")
}
