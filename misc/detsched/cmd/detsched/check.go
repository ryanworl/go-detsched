// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The mutation check (-check): whether the harness can fail. It plants
// one concurrency or durability mistake at a time in the code under test,
// at code that passing runs reached, and runs a short search of each
// mutant: a harness that catches few of them would miss real bugs of the
// same kinds, however long its searches run clean.

const (
	checkMutants = 20  // mutants that compile, at most
	checkRuns    = 100 // runs of each search, without -n
	checkCover   = 8   // seeds whose coverage picks the sites
)

// A mutation is one mistake: edits to the files of one package.
type mutation struct {
	op    string // the operator, for spreading the mutants over them
	file  string // absolute path, where it is reported
	name  string // file's path in its module, as reports name it
	line  int
	desc  string // such as "drop the Lock and Unlock of s.mu"
	edits []edit
	ran   []site // the code it changes: passing runs must have run one
}

// An edit replaces the source bytes [off, end) of file with text.
type edit struct {
	file     string
	off, end int
	text     string
}

// A site is a line of a file.
type site struct {
	file string
	line int
}

func (m *mutation) where() string { return fmt.Sprintf("%s:%d", m.name, m.line) }

// files returns the files m edits.
func (m *mutation) files() []string {
	var fs []string
	for _, e := range m.edits {
		if !slices.Contains(fs, e.file) {
			fs = append(fs, e.file)
		}
	}
	return fs
}

// apply returns src, the source of file, with m's edits to it.
func (m *mutation) apply(file string, src []byte) []byte {
	es := slices.DeleteFunc(slices.Clone(m.edits), func(e edit) bool { return e.file != file })
	slices.SortFunc(es, func(a, b edit) int { return b.off - a.off })
	out := slices.Clone(src)
	for _, e := range es {
		out = slices.Concat(out[:e.off], []byte(e.text), out[e.end:])
	}
	return out
}

// preferred reports whether op is a concurrency or durability mistake,
// which the check plants before comparison and error-check flips.
func preferred(op string) bool { return op != "compare" && op != "error" }

// mutationCheck runs the mutation check of the tests of pkg that run selects,
// with n runs in each search, and returns the exit status.
func mutationCheck(ctx context.Context, pkg, run string, n uint64, workers int, timeout time.Duration, w io.Writer) (int, error) {
	if n == 0 {
		n = checkRuns
	}
	out, err := os.MkdirTemp("", "detsched-check-")
	if err != nil {
		return 2, err
	}
	defer os.RemoveAll(out)
	progress := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "detsched -check: "+format+"\n", args...) }

	// search runs n runs of bin, built in dir, and returns the search,
	// which a mutant's stops at its first failure.
	search := func(dir, bin string, mutant bool) (*search, error) {
		s, err := newSearch("", bin, run)
		if err != nil {
			return nil, err
		}
		s.dir, s.workers, s.timeout, s.inCheck = dir, workers, timeout, true
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			for mutant && sctx.Err() == nil {
				s.mu.Lock()
				failed := len(s.groups) > 0
				s.mu.Unlock()
				if failed {
					cancel()
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		err = s.run(sctx, n)
		return s, err
	}

	// The program must pass without mutations, so that a mutant's
	// failure is the mutation's.
	progress("searching the program without mutations, %d runs", n)
	if err := os.MkdirAll(filepath.Join(out, "base"), 0o777); err != nil {
		return 2, err
	}
	dir, bin, err := buildPackage(pkg, filepath.Join(out, "base"))
	if err != nil {
		return 2, err
	}
	base, err := search(dir, bin, false)
	if err != nil {
		return 2, err
	}
	if len(base.groups) > 0 {
		// Its logs and binary stay, for its replay lines.
		base.summary(w, true)
		fmt.Fprintf(w, "\ndetsched -check: the program fails without mutations; fix these failures first, since a mutant's failures could not be told from them\n")
		return 1, nil
	}
	os.RemoveAll(base.out)

	// The sites: the code under test that passing runs reached.
	pkgs, mods, err := checkPackages(pkg)
	if err != nil {
		return 2, err
	}
	if len(pkgs) == 0 {
		fmt.Fprintf(w, "detsched -check: no code to mutate: the test binary links no non-test Go files of %s; the code under test is the tests' module and the modules it replaces with a local directory (go.mod's replace), but detsched's SDK\n", strings.Join(mods, ", "))
		return 2, nil
	}
	reached, err := checkCoverage(ctx, pkg, run, pkgs, filepath.Join(out, "cover"))
	if err != nil {
		return 2, err
	}
	if reached == nil {
		var paths []string
		for _, p := range pkgs {
			paths = append(paths, p.path)
		}
		fmt.Fprintf(w, "detsched -check: no mistake to plant: runs of seeds 1 to %d reached none of the code under test, in packages %s\n", checkCover, strings.Join(paths, ", "))
		return 2, nil
	}
	var muts []*mutation
	for _, p := range pkgs {
		ms, err := mutations(p.files)
		if err != nil {
			return 2, err
		}
		for _, m := range ms {
			m.name = path.Join(p.dir, filepath.Base(m.file))
			if slices.ContainsFunc(m.ran, func(s site) bool { return reached(s.file, s.line) }) {
				muts = append(muts, m)
			}
		}
	}
	if len(muts) == 0 {
		fmt.Fprintf(w, "detsched -check: no mistake to plant: the runs reached no lock, channel, close, comparison, error check, or Sync in the code under test\n")
		return 2, nil
	}

	// The mutants, spread over the operators, each a short search:
	// concurrency and durability mistakes first.
	rng := rand.New(rand.NewPCG(1, 2))
	byOp := map[string][]*mutation{}
	var ops []string
	for _, m := range muts {
		if byOp[m.op] == nil {
			ops = append(ops, m.op)
		}
		byOp[m.op] = append(byOp[m.op], m)
	}
	slices.SortStableFunc(ops, func(a, b string) int {
		switch {
		case preferred(a) == preferred(b):
			return 0
		case preferred(a):
			return -1
		}
		return 1
	})
	type outcome struct {
		m      *mutation
		caught string // the first failure, or ""
	}
	var outcomes []outcome
	skipped := 0
	for i := 0; len(outcomes) < checkMutants && len(ops) > 0; i++ {
		// Round robin over the preferred operators while any has
		// mutants left, then over the others.
		live := ops
		if preferred(ops[0]) {
			live = slices.DeleteFunc(slices.Clone(ops), func(o string) bool { return !preferred(o) })
		}
		op := live[i%len(live)]
		ms := byOp[op]
		k := rng.IntN(len(ms))
		m := ms[k]
		byOp[op] = slices.Delete(ms, k, k+1)
		if len(byOp[op]) == 0 {
			ops = slices.DeleteFunc(ops, func(o string) bool { return o == op })
			i--
		}
		mdir := filepath.Join(out, fmt.Sprintf("m%d", len(outcomes)+skipped+1))
		bin, err := buildMutant(pkg, mdir, m)
		if err != nil {
			skipped++ // such as a variable left unused
			continue
		}
		progress("mutant %d of at most %d: %s %s", len(outcomes)+1, checkMutants, m.where(), m.desc)
		s, err := search(dir, bin, true)
		if err != nil {
			return 2, err
		}
		os.RemoveAll(s.out)
		o := outcome{m: m}
		if len(s.groups) > 0 {
			o.caught, _ = splitSignature(s.groups[0].sig)
		}
		outcomes = append(outcomes, o)
		if ctx.Err() != nil {
			break
		}
	}
	caught := 0
	for _, o := range outcomes {
		if o.caught != "" {
			caught++
		}
	}
	fmt.Fprintf(w, "detsched -check: caught %d of %d mistakes planted in code the passing runs reached, with %d runs each", caught, len(outcomes), n)
	switch {
	case skipped == 1:
		fmt.Fprintf(w, " (1 more did not compile and was skipped)")
	case skipped > 1:
		fmt.Fprintf(w, " (%d more did not compile and were skipped)", skipped)
	}
	fmt.Fprintf(w, "\n")
	for _, o := range outcomes {
		if o.caught == "" {
			fmt.Fprintf(w, "\tsurvived: %s %s\n", o.m.where(), o.m.desc)
		}
	}
	for _, o := range outcomes {
		if o.caught != "" {
			fmt.Fprintf(w, "\tcaught:   %s %s: %s\n", o.m.where(), o.m.desc, truncate(o.caught, 200))
		}
	}
	if caught < len(outcomes) {
		fmt.Fprintf(w, "detsched -check: the tests cannot see the survivors: make the workload run that code concurrently and under faults, and assert what the code must keep. A survivor that breaks nothing the code promises, such as a buffer it does not depend on, needs nothing\n")
		return 1, nil
	}
	return 0, nil
}

// A checkPackage is a package of the code under test.
type checkPackage struct {
	path  string
	dir   string   // its directory in its module, such as "internal/store"
	files []string // its non-test Go files, absolute
}

// checkPackages returns the packages of the code under test that pkg's
// test binary links, with their non-test Go files: those of the tests'
// module and of the modules it replaces with a local directory, as a
// harness module does with its copy of the code it tests, but detsched's
// SDK. It also returns the modules it looked at, for a message when there
// is nothing to mutate.
func checkPackages(pkg string) ([]checkPackage, []string, error) {
	list := exec.Command(goCmd(), "list", "-deps", "-test", "-f",
		`{{if .Module}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{.Module.Path}}{{"\t"}}{{.Module.Main}}{{"\t"}}{{with .Module.Replace}}{{if not .Version}}{{.Path}}{{end}}{{end}}{{"\t"}}{{join .GoFiles " "}}{{"\t"}}{{join .TestGoFiles " "}}{{end}}`, pkg)
	list.Env = catalogEnv()
	b, err := list.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list %s: %v", pkg, err)
	}
	var ps []checkPackage
	var mods []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			continue
		}
		path, dir, mod, main, local := f[0], f[1], f[2], f[3] == "true", f[4]
		// The package under test is listed as built for its test, with
		// its test files among its Go files.
		path, _, _ = strings.Cut(path, " ")
		if !main && local == "" || strings.HasPrefix(path, sdkPrefix) || strings.HasSuffix(path, ".test") {
			continue
		}
		m := mod + " (the tests' module)"
		if !main {
			m = mod + " (replaced by " + local + ")"
		}
		if !slices.Contains(mods, m) {
			mods = append(mods, m)
		}
		test := strings.Fields(f[6])
		var files []string
		for _, name := range strings.Fields(f[5]) {
			if !slices.Contains(test, name) {
				files = append(files, filepath.Join(dir, name))
			}
		}
		if len(files) == 0 || slices.ContainsFunc(ps, func(p checkPackage) bool { return p.path == path }) {
			continue
		}
		ps = append(ps, checkPackage{path: path, dir: strings.TrimPrefix(strings.TrimPrefix(path, mod), "/"), files: files})
	}
	return ps, mods, nil
}

// checkCoverage runs the first checkCover seeds of pkg built with
// coverage of pkgs, and returns whether a line of a file ran, or nil if
// none did.
func checkCoverage(ctx context.Context, pkg, run string, pkgs []checkPackage, out string) (func(file string, line int) bool, error) {
	var paths []string
	dirOf := map[string]string{} // import path to directory
	for _, p := range pkgs {
		paths = append(paths, p.path)
		dirOf[p.path] = filepath.Dir(p.files[0])
	}
	if err := os.MkdirAll(out, 0o777); err != nil {
		return nil, err
	}
	dir, bin, err := buildPackage(pkg, out, "-cover", "-coverpkg="+strings.Join(paths, ","))
	if err != nil {
		return nil, err
	}
	// The test binary writes its profile itself: the runtime's own
	// coverage files (GOCOVERDIR) are not written in det mode.
	var profiles []string
	for seed := 1; seed <= checkCover; seed++ {
		prof := filepath.Join(out, fmt.Sprintf("cover-%d.txt", seed))
		args := []string{"-test.coverprofile=" + prof}
		if run != "" {
			args = append(args, "-test.run="+run)
		}
		cmd := exec.CommandContext(ctx, bin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "PWD="+dir, "GODETSEED="+strconv.Itoa(seed))
		cmd.Run() // a run that fails covers what it reached
		profiles = append(profiles, prof)
	}
	type span struct{ from, to int }
	ran := map[string][]span{}
	wrote := false
	for _, prof := range profiles {
		b, err := os.ReadFile(prof)
		if err != nil {
			continue
		}
		wrote = true
		for _, line := range strings.Split(string(b), "\n") {
			// path/file.go:L1.C1,L2.C2 STMTS COUNT
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[2] == "0" {
				continue
			}
			name, pos, ok := strings.Cut(fields[0], ":")
			if !ok {
				continue
			}
			file := filepath.Join(dirOf[filepath.Dir(name)], filepath.Base(name))
			a, b, _ := strings.Cut(pos, ",")
			l1, _ := strconv.Atoi(a[:strings.IndexByte(a+".", '.')])
			l2, _ := strconv.Atoi(b[:strings.IndexByte(b+".", '.')])
			ran[file] = append(ran[file], span{l1, l2})
		}
	}
	if !wrote {
		return nil, fmt.Errorf("no run of %s wrote its coverage profile; -check needs a package with tests", pkg)
	}
	if len(ran) == 0 {
		return nil, nil
	}
	return func(file string, line int) bool {
		return slices.ContainsFunc(ran[file], func(s span) bool { return s.from <= line && line <= s.to })
	}, nil
}

// buildMutant builds pkg's test binary in dir with m applied, through an
// overlay, so that the source is untouched.
func buildMutant(pkg, dir string, m *mutation) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	replace := map[string]string{}
	for i, file := range m.files() {
		src, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		mutated := filepath.Join(dir, fmt.Sprintf("%d-%s", i, filepath.Base(file)))
		if err := os.WriteFile(mutated, m.apply(file, src), 0o666); err != nil {
			return "", err
		}
		replace[file] = mutated
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": replace})
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, ov, 0o666); err != nil {
		return "", err
	}
	_, bin, err := buildPackage(pkg, dir, "-overlay="+overlay)
	return bin, err
}

// A lockCall is a statement that locks or unlocks a mutex: x.Lock(),
// x.Unlock(), x.RLock(), x.RUnlock(), or a defer of one.
type lockCall struct {
	file   string
	line   int
	off    int
	end    int
	x      string // the mutex, x
	lock   bool   // Lock or RLock
	paired bool   // a Lock with its Unlock later in the same block
}

// mutations returns the mistakes the check can plant in files, the
// non-test files of one package.
func mutations(files []string) ([]*mutation, error) {
	var ms []*mutation
	locks := map[string][]lockCall{} // by the mutex's name, its last selector
	for _, file := range files {
		fms, err := fileMutations(file, locks)
		if err != nil {
			return nil, err
		}
		ms = append(ms, fms...)
	}
	// Every Lock and Unlock of one mutex across the package, for a
	// mutex locked in one function and unlocked in another, which the
	// pair in one block misses, or locked in several places.
	for _, name := range slices.Sorted(maps.Keys(locks)) {
		cs := locks[name]
		var lockSites []site
		unpaired := false
		for _, c := range cs {
			if c.lock {
				lockSites = append(lockSites, site{c.file, c.line})
				unpaired = unpaired || !c.paired
			}
		}
		if len(lockSites) == 0 || !unpaired && len(lockSites) < 2 {
			continue
		}
		m := &mutation{op: "lockall", file: lockSites[0].file, line: lockSites[0].line, ran: lockSites,
			desc: fmt.Sprintf("drop every Lock and Unlock of %s, %d calls", name, len(cs))}
		for _, c := range cs {
			// Keep the mutex used, so that the mutant compiles.
			m.edits = append(m.edits, edit{c.file, c.off, c.end, "_ = &" + c.x})
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// fileMutations returns the mistakes the check can plant in file alone,
// and adds its lock calls to locks.
func fileMutations(file string, locks map[string][]lockCall) ([]*mutation, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, nil // the build reports it
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	text := func(n ast.Node) string { return string(src[off(n.Pos()):off(n.End())]) }
	line := func(n ast.Node) int { return fset.Position(n.Pos()).Line }
	var ms []*mutation
	// add adds a mutation at at, which changes code on the line of ran.
	add := func(op string, at, ran ast.Node, desc string, es ...edit) {
		ms = append(ms, &mutation{op: op, file: file, line: line(at), desc: desc, edits: es, ran: []site{{file, line(ran)}}})
	}
	drop := func(n ast.Node) edit { return edit{file, off(n.Pos()), off(n.End()), ""} }
	// lockName returns the name locks files a mutex under: the last
	// selector of x, as "mu" for c.mu, or "" for another expression.
	lockName := func(x ast.Expr) string {
		switch x := x.(type) {
		case *ast.SelectorExpr:
			return x.Sel.Name
		case *ast.Ident:
			return x.Name
		}
		return ""
	}
	// method returns the receiver of a call x.name() without arguments.
	method := func(n ast.Node, names ...string) (ast.Expr, string) {
		var call *ast.CallExpr
		switch n := n.(type) {
		case *ast.ExprStmt:
			call, _ = n.X.(*ast.CallExpr)
		case *ast.DeferStmt:
			call = n.Call
		}
		if call == nil || len(call.Args) > 0 {
			return nil, ""
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !slices.Contains(names, sel.Sel.Name) {
			return nil, ""
		}
		return sel.X, sel.Sel.Name
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			// A Lock and the Unlock after it in the same block.
			for i, st := range n.List {
				x, name := method(st, "Lock", "RLock", "Unlock", "RUnlock")
				if x == nil || lockName(x) == "" {
					continue
				}
				c := lockCall{file: file, line: line(st), off: off(st.Pos()), end: off(st.End()), x: text(x), lock: name == "Lock" || name == "RLock"}
				if c.lock {
					for _, st2 := range n.List[i+1:] {
						if y, uname := method(st2, "Unlock", "RUnlock"); y != nil && text(y) == text(x) && uname == "R"[:len(name)-4]+"Unlock" {
							add("lock", st, st, fmt.Sprintf("drop the %s and %s of %s", name, uname, text(x)), drop(st), drop(st2))
							c.paired = true
							break
						}
					}
				}
				locks[lockName(x)] = append(locks[lockName(x)], c)
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "make" && len(n.Args) >= 1 {
				if ct, ok := n.Args[0].(*ast.ChanType); ok {
					switch len(n.Args) {
					case 1:
						add("chan", n, n, fmt.Sprintf("buffer %s", text(n)), edit{file, off(n.Rparen), off(n.Rparen), ", 1"})
					case 2:
						if lit, ok := n.Args[1].(*ast.BasicLit); !ok || lit.Value != "0" {
							add("chan", n, n, fmt.Sprintf("unbuffer %s", text(n)), edit{file, off(ct.End()), off(n.Args[1].End()), ""})
						}
					}
				}
			}
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sync" && len(n.Args) == 0 {
				add("sync", n, n, fmt.Sprintf("drop %s, as if it succeeded", text(n)), edit{file, off(n.Pos()), off(n.End()), "error(nil)"})
			}
		case *ast.ExprStmt:
			if call, ok := n.X.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "close" && len(call.Args) == 1 {
					add("close", n, n, fmt.Sprintf("drop %s", text(n)), drop(n))
				}
			}
		case *ast.BinaryExpr:
			flip := map[token.Token]string{token.LSS: "<=", token.LEQ: "<", token.GTR: ">=", token.GEQ: ">"}
			if to, ok := flip[n.Op]; ok {
				add("compare", n, n, fmt.Sprintf("%s to %s", text(n), to), edit{file, off(n.OpPos), off(n.OpPos) + len(n.Op.String()), to})
			}
		case *ast.IfStmt:
			// if err != nil { ... return ... }
			b, ok := n.Cond.(*ast.BinaryExpr)
			if !ok || b.Op != token.NEQ {
				break
			}
			x, ok1 := b.X.(*ast.Ident)
			y, ok2 := b.Y.(*ast.Ident)
			if !ok1 || !ok2 || y.Name != "nil" || !strings.HasSuffix(strings.ToLower(x.Name), "err") {
				break
			}
			// The error path must have run: ignoring an error that
			// never came changes nothing.
			if len(n.Body.List) == 0 {
				break
			}
			add("error", n, n.Body.List[0], fmt.Sprintf("ignore the error in %s", text(b)), edit{file, off(b.Pos()), off(b.Pos()), "false && "})
		}
		return true
	})
	return ms, nil
}
