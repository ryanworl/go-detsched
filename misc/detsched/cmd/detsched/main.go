// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Detsched looks for schedules in which a package's tests fail, and prints
// a command that replays each failure.
//
// Usage:
//
//	detsched [-run regexp] [-duration d] [-n seeds] [-timeout d] [-p n] [package]
//
// Detsched builds the tests of the package (by default, the one in the
// current directory) with GOEXPERIMENT=detsched, or the package itself if
// it is a command without tests, and runs the binary once per seed
// (GODETSEED=1, 2, ...), one run per core at once (-p sets how many), until
// it is interrupted, -duration passes, or -n seeds have run. Each seed runs
// another schedule, and with package detsched/sdk/sim, other faults. It
// groups the failing runs by their first assertion failure, panic, or fatal
// error, else their first failing test, keeps the output of the first few
// runs of each group, and prints a command that replays the group's first
// run: the same binary, directory, arguments, and seed, with GODETBRANCH
// naming the branch of a detsched/sdk/compose timeline that failed. It runs
// that command to check that the failure reproduces. To debug the failure,
// run the command with dlv exec in front of the binary and -- before its
// arguments.
//
// A run that takes longer than -timeout (default 10m) is stopped with
// SIGQUIT, which prints its goroutines, and counts as a failure.
//
// Detsched exits with status 1 if any run failed.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	procs    = flag.Int("p", runtime.NumCPU(), "run this many programs at once")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: detsched [flags] [package]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}
	pkg := "."
	if flag.NArg() == 1 {
		pkg = flag.Arg(0)
	}
	s, err := newSearch(pkg, *runFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detsched: %v\n", err)
		os.Exit(2)
	}
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	go func() {
		<-ctx.Done()
		stop() // a second interrupt kills detsched
	}()
	err = s.run(ctx, *seeds)
	s.summary(os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "detsched: %v\n", err)
		os.Exit(2)
	}
	if len(s.groups) > 0 {
		os.Exit(1)
	}
	os.RemoveAll(s.out) // nothing to replay
}

// A search runs a test binary with one seed after another.
type search struct {
	pkg     string
	dir     string   // the package's directory, where runs start
	bin     string   // the binary
	args    []string // its arguments
	env     []string // variables every run adds to the environment
	vars    []string // variables of our environment the schedule depends on, which replay lines set
	out     string   // directory for the binary and the logs
	timeout time.Duration
	workers int

	progress io.Writer // where to report progress, or nil
	live     bool      // progress is a terminal, where a status line updates in place

	// Set up by prepare.
	setup       bool   // the program reports the end of its setup
	stop        uint64 // the switch right after setup, which setup roots stop at
	maxSwitches uint64 // maxswitches of forked runs
	novelty     string // what counts as a new feature
	seen        []byte // features recorded so far, shared with every fork
	seenFile    *os.File

	mu         sync.Mutex
	start      time.Time
	rng        *rand.Rand
	runs       int
	failed     int
	tooLong    int           // runs stopped for too many switches, not counted as failures
	slowPass   time.Duration // the longest passing run
	slowFail   time.Duration // the longest failing run, timeouts aside
	groups     []*group      // in order of first appearance
	byKey      map[string]*group
	arms       [numArms]arm
	cells      []*cell      // the archive
	lru        []*cell      // cells with stopped programs, least recently used first
	pool       []*cell      // setup roots
	claimed    map[int]bool // new features of plain runs' cells not stopped yet
	kept       int          // cells when the search ended
	lowMem     bool         // memory ran low, which has been reported
	memWaited  time.Time    // when waitMem last reported waiting
	baseTrace  string       // the trace line of the base run, seed 1
	unstable   string       // how seed 1 ran two schedules, or ""
	left       int          // places where seed 1 left the deterministic subset
	only       int          // the only arm to run, for measuring, or -1
	noFork     bool         // run fresh seeds without forking, for measuring
	roots      map[int]bool // process groups of the fork servers
	running    map[*liveRun]bool
	outOfMem   int // runs stopped because memory ran out, not counted as failures
	features   int // new features recorded during the search
	canFork    bool
	forked     bool   // a fork succeeded
	refused    int    // forks the program refused
	refusal    string // why the search stopped forking, or ""
	yieldSites []string
	predicted  []string // deadlocks the lock order predicts
}

// A group is the failing runs with one signature.
type group struct {
	key    string   // the signature with numbers masked
	sig    string   // the signature of the first run
	runs   int      // runs that failed this way
	logs   []string // the kept logs
	replay string   // the command that replays the first run
	debug  string   // the same under Delve
	status string   // whether running replay reproduced the failure
}

// keepLogs is how many logs of each group a search keeps.
const keepLogs = 10

// newSearch builds pkg in a new directory.
func newSearch(pkg, run string) (*search, error) {
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
		roots:   map[int]bool{},
		running: map[*liveRun]bool{},
		claimed: map[int]bool{},
		rng:     rand.New(rand.NewPCG(1, 2)),
		only:    -1,
	}
	s.dir, s.bin, err = buildPackage(pkg, out)
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

func isFile(name string) bool {
	fi, err := os.Stat(name)
	return err == nil && fi.Mode().IsRegular()
}

// buildPackage builds pkg's test binary, or pkg itself if it is a command
// without tests, with the experiment and without cgo, which a process
// that forks a branch of a timeline must not use. It returns the
// package's directory and the binary.
func buildPackage(pkg, out string) (dir, bin string, err error) {
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
	wall        time.Duration
	outOfMem    bool // stopped because memory ran out
}

var (
	traceLine     = regexp.MustCompile(`(?m)^detsched: trace=0x[0-9a-f]+ switches=\d+ syncs=\d+$`)
	switchesField = regexp.MustCompile(` switches=(\d+) `)
	syncsField    = regexp.MustCompile(` syncs=(\d+)$`)
	setupLine     = regexp.MustCompile(`(?m)^detsched: setup complete at switch (\d+)$`)
	deadlockLine  = regexp.MustCompile(`(?m)^detsched: potential deadlock: (.*)$`)
	directiveLine = regexp.MustCompile(`(?m)^detsched: deadlock directive yield=(\S+)$`)
	leftReport    = regexp.MustCompile(`(?m)^detsched: .*; the program no longer runs deterministically\n(?:.+\n)*`) // and its stack
	failureLine   = regexp.MustCompile(`(?m)^(detsched: assertion failed: |panic: |fatal error: ).*`)
	testFailure   = regexp.MustCompile(`(?m)^\s*--- FAIL: \S+`)
	branchSet     = regexp.MustCompile(`GODETBRANCH=(\d+)`)
	assertDetails = regexp.MustCompile(`^detsched: assertion failed: .*?( map\[| \(and \d+ more failures?\)$)`)
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

// signature returns the line that tells how the run failed: its first
// assertion failure, panic, or fatal error (a branch of a composer
// timeline that fails writes its output first), else its first failing
// test, else "timeout" or its exit status.
func (r *result) signature() string {
	line := failureLine.Find(r.output)
	if line == nil {
		line = bytes.TrimSpace(testFailure.Find(r.output))
	}
	switch {
	case line != nil:
		return truncate(string(line), 200)
	case r.timedOut:
		return "timeout"
	}
	return fmt.Sprintf("exit status %d", r.exitCode)
}

// key returns the signature with the numbers in it masked, so that one
// bug makes one group.
// An assertion failure's key is its message, without its details or the
// count of other failures.
func (r *result) key() string {
	sig := r.signature()
	if m := assertDetails.FindStringIndex(sig); m != nil {
		sig = sig[:m[0]]
	}
	return masked.ReplaceAllString(sig, "N")
}

// branch returns the number of the composer branch the failure happened
// in, or 0 for the timeline itself.
func (r *result) branch() int {
	m := branchSet.FindSubmatch(r.output)
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
// breakpoints.
func (s *search) replayLine(r *result, debug bool) string {
	env := []string{fmt.Sprintf("GODETSEED=%d", r.seed), fmt.Sprintf("GODETBRANCH=%d", r.branch())}
	if r.tape != "" {
		env = addSearch(env, "tape="+r.tape)
	}
	env = addSearch(env, yieldField(r.yield)...)
	var b strings.Builder
	fmt.Fprintf(&b, "cd %s && ", quote(s.dir))
	for _, e := range slices.Concat(env, r.env, s.vars) {
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
	return b.String()
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

// errCanceled reports a run stopped because the search ended.
var errCanceled = errors.New("canceled")

// exec runs cmd in its own process group, which holds the branches it
// forks, and stops the group when the run takes longer than s.timeout or
// ctx is done.
func (s *search) exec(ctx context.Context, cmd *exec.Cmd) (*result, error) {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second // a branch left behind may hold the output open
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	defer syscall.Kill(-pgid, syscall.SIGKILL)
	untrack := s.track(start, func() { syscall.Kill(-pgid, syscall.SIGKILL) })
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
		syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		return nil, errCanceled
	case <-timer.C:
		r.timedOut = true
		syscall.Kill(-pgid, syscall.SIGQUIT)
		select {
		case err = <-done:
		case <-time.After(10 * time.Second):
			syscall.Kill(-pgid, syscall.SIGKILL)
			err = <-done
		}
	}
	r.wall = time.Since(start)
	r.outOfMem = untrack()
	r.output = out.Bytes()
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
	if err := s.prepare(ctx); err != nil {
		if errors.Is(err, errCanceled) {
			return nil
		}
		return err
	}
	go s.guardMem(ctx)
	jobs := make(chan uint64)
	go func() {
		defer close(jobs)
		for seed := uint64(2); n == 0 || seed <= n; seed++ {
			select {
			case jobs <- seed:
			case <-ctx.Done():
				return
			}
		}
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
	if firstErr == nil {
		s.recheck()
	}
	return firstErr
}

// recheck runs seed 1 once more, minutes after the base run in real time,
// and warns if it runs another schedule: a program that reads the real
// clock outside a bubble, such as at init, often runs the same schedule
// in two runs started at once.
func (s *search) recheck() {
	r, err := s.exec(context.Background(), s.plain(1))
	if err != nil || r.trace() == s.baseTrace {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unstable = fmt.Sprintf("seed 1 ran two different schedules (%q at the start of the search, %q at its end), so failures may not replay; the program depends on something besides its seed, such as real time or the OS", s.baseTrace, r.trace())
	s.printf("detsched: warning: %s\n", s.unstable)
}

// record records a run, and when it is the first of its group, replays it.
func (s *search) record(ctx context.Context, r *result, a int) error {
	r.arm = a
	if r.env == nil {
		r.env = s.env
	}
	s.mu.Lock()
	s.predict(r.output)
	s.runs++
	if r.tooLong() {
		s.tooLong++
		s.mu.Unlock()
		return nil
	}
	if r.outOfMem {
		s.outOfMem++
		s.mu.Unlock()
		return nil
	}
	if !r.failed() {
		s.slowPass = max(s.slowPass, r.wall)
		s.mu.Unlock()
		return nil
	}
	s.failed++
	if !r.timedOut {
		s.slowFail = max(s.slowFail, r.wall)
	}
	g := s.byKey[r.key()]
	first := g == nil
	if first {
		g = &group{key: r.key(), sig: r.signature(), replay: s.replayLine(r, false), debug: s.replayLine(r, true), status: "replaying"}
		s.byKey[g.key] = g
		s.groups = append(s.groups, g)
	}
	g.runs++
	var log string
	if len(g.logs) < keepLogs {
		log = s.temp(fmt.Sprintf("fail-%d.log", s.failed))
		g.logs = append(g.logs, log)
	}
	if first {
		s.printf("FAIL %s\n\tlog:    %s\n\treplay: %s\n\tdebug:  %s\n", g.sig, log, g.replay, g.debug)
	}
	s.mu.Unlock()

	if log != "" {
		s.mu.Lock()
		line := s.replayLine(r, false)
		s.mu.Unlock()
		data := append([]byte("# "+line+"\n"), r.output...)
		if err := os.WriteFile(log, data, 0o666); err != nil {
			return err
		}
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
			s.printf("detsched: the replay of %q needs %s:\n\treplay: %s\n\tdebug:  %s\n", g.sig, strings.Join(vars, " "), g.replay, g.debug)
			s.mu.Unlock()
			if log != "" {
				if err := os.WriteFile(log, append([]byte("# "+g.replay+"\n"), r.output...), 0o666); err != nil {
					return err
				}
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
	s.printf("detsched: the replay of %q %s\n", g.sig, verdict(status))
	s.mu.Unlock()
	return nil
}

// verdict returns how a replay's status reads after "the replay".
func verdict(status string) string {
	if status == "reproduced" {
		return "reproduced the failure in a new shell"
	}
	return status
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
	switch {
	case again.key() != r.key():
		return fmt.Sprintf("did not reproduce the failure: it ended with %q", again.signature()), nil
	case again.trace() != r.trace():
		return fmt.Sprintf("failed the same way, but ran another schedule (%q, not %q)", again.trace(), r.trace()), nil
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

// status returns a line of counts. s.mu must be held.
func (s *search) status() string {
	d := time.Since(s.start)
	return fmt.Sprintf("%d runs, %d failed, %d signatures, %d features, %.1f runs/s, %v", s.runs, s.failed, len(s.groups), s.features, float64(s.runs)/d.Seconds(), d.Round(time.Second))
}

// report prints the status line every second on a terminal, else every
// 30 seconds, until the returned function is called.
func (s *search) report() (stop func()) {
	every := 30 * time.Second
	if s.live {
		every = time.Second
	}
	t := time.NewTicker(every)
	done := make(chan bool)
	go func() {
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			s.mu.Lock()
			if s.live {
				fmt.Fprint(s.progress, "\r\033[K"+s.status())
			} else {
				fmt.Fprintln(s.progress, "detsched: "+s.status())
			}
			s.mu.Unlock()
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

// summary prints the counts, the search's mix, and every group.
func (s *search) summary(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(w, "detsched: %s: %s\n", s.pkg, s.status())
	if s.unstable != "" {
		fmt.Fprintf(w, "detsched: warning: %s\n", s.unstable)
	}
	if s.left > 0 {
		places := fmt.Sprintf("%d places", s.left)
		if s.left == 1 {
			places = "1 place"
		}
		fmt.Fprintf(w, "detsched: warning: seed 1 left the deterministic subset in %s, printed when the search started, so failures may not replay\n", places)
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
		fmt.Fprintf(w, "detsched: runs by start: %s; %d cells kept; new features: %s\n", strings.Join(mix, ", "), s.kept, s.novelty)
	}
	if s.refusal != "" {
		fmt.Fprintf(w, "detsched: the program refused to fork (%s), so the search ran fresh seeds only\n", s.refusal)
	}
	for _, p := range s.predicted {
		fmt.Fprintf(w, "detsched: predicted deadlock: %s\n", p)
	}
	if s.outOfMem > 0 {
		fmt.Fprintf(w, "detsched: %d runs were stopped because memory ran out, not counted as failures\n", s.outOfMem)
	}
	if s.tooLong > 0 {
		fmt.Fprintf(w, "detsched: %d runs made 1000 times the switches of the first and were stopped, not counted as failures\n", s.tooLong)
	}
	fmt.Fprintf(w, "detsched: slowest passing run %v, slowest failing run %v; -timeout is %v\n", s.slowPass.Round(time.Millisecond), s.slowFail.Round(time.Millisecond), s.timeout)
	for _, g := range s.groups {
		fmt.Fprintf(w, "\nFAIL %d of %d runs: %s\n", g.runs, s.runs, g.sig)
		more := ""
		if len(g.logs) > 1 {
			more = fmt.Sprintf(" (and %d more in %s)", len(g.logs)-1, s.out)
		}
		fmt.Fprintf(w, "\tlog:    %s%s\n", g.logs[0], more)
		fmt.Fprintf(w, "\treplay: %s\n\tdebug:  %s\n", g.replay, g.debug)
		fmt.Fprintf(w, "\tthe replay %s\n", verdict(g.status))
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("TERM") != "dumb"
}
