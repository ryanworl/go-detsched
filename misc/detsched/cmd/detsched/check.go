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
	"math/rand/v2"
	"os"
	"os/exec"
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

// A mutation is one mistake: edits to one file.
type mutation struct {
	op    string // the operator, for spreading the mutants over them
	file  string // absolute path
	line  int
	desc  string // such as "drop the Lock and Unlock of s.mu"
	edits []edit
}

// An edit replaces the source bytes [off, end) with text.
type edit struct {
	off, end int
	text     string
}

func (m *mutation) where() string { return fmt.Sprintf("%s:%d", filepath.Base(m.file), m.line) }

// apply returns src with m's edits.
func (m *mutation) apply(src []byte) []byte {
	es := slices.Clone(m.edits)
	slices.SortFunc(es, func(a, b edit) int { return b.off - a.off })
	out := slices.Clone(src)
	for _, e := range es {
		out = slices.Concat(out[:e.off], []byte(e.text), out[e.end:])
	}
	return out
}

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
	pkgs, err := checkPackages(pkg)
	if err != nil {
		return 2, err
	}
	if len(pkgs) == 0 {
		fmt.Fprintf(w, "detsched -check: no code to mutate: the tests' module has no non-test Go files besides detsched's SDK\n")
		return 2, nil
	}
	reached, err := checkCoverage(ctx, pkg, run, pkgs, filepath.Join(out, "cover"))
	if err != nil {
		return 2, err
	}
	var muts []*mutation
	for _, p := range pkgs {
		for _, f := range p.files {
			ms, err := mutations(f)
			if err != nil {
				return 2, err
			}
			for _, m := range ms {
				if reached(m.file, m.line) {
					muts = append(muts, m)
				}
			}
		}
	}
	if len(muts) == 0 {
		fmt.Fprintf(w, "detsched -check: no mistake to plant: the runs reached no lock, channel, close, comparison, error check, or Sync in the code under test\n")
		return 2, nil
	}

	// The mutants, spread over the operators, each a short search.
	rng := rand.New(rand.NewPCG(1, 2))
	byOp := map[string][]*mutation{}
	var ops []string
	for _, m := range muts {
		if byOp[m.op] == nil {
			ops = append(ops, m.op)
		}
		byOp[m.op] = append(byOp[m.op], m)
	}
	type outcome struct {
		m      *mutation
		caught string // the first failure, or ""
	}
	var outcomes []outcome
	skipped := 0
	for i := 0; len(outcomes) < checkMutants && len(ops) > 0; i++ {
		op := ops[i%len(ops)]
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
	files []string // its non-test Go files, absolute
}

// checkPackages returns the packages of pkg's module that its test binary
// links, but detsched's SDK, with their non-test Go files.
func checkPackages(pkg string) ([]checkPackage, error) {
	list := exec.Command(goCmd(), "list", "-deps", "-test", "-f",
		`{{if and .Module .Module.Main}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{join .GoFiles " "}}{{end}}`, pkg)
	list.Env = catalogEnv()
	b, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %v", pkg, err)
	}
	var ps []checkPackage
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) == 3 {
			// The package under test is listed as built for its test.
			f[0], _, _ = strings.Cut(f[0], " ")
		}
		if len(f) != 3 || strings.HasPrefix(f[0], sdkPrefix) || strings.HasSuffix(f[0], ".test") || f[2] == "" || slices.ContainsFunc(ps, func(p checkPackage) bool { return p.path == f[0] }) {
			continue
		}
		p := checkPackage{path: f[0]}
		for _, name := range strings.Fields(f[2]) {
			p.files = append(p.files, filepath.Join(f[1], name))
		}
		ps = append(ps, p)
	}
	return ps, nil
}

// checkCoverage runs the first checkCover seeds of pkg built with
// coverage of pkgs, and returns whether a line of a file ran.
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
	for _, prof := range profiles {
		b, err := os.ReadFile(prof)
		if err != nil {
			continue
		}
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
	if len(ran) == 0 {
		return nil, fmt.Errorf("no run of %s wrote coverage; -check needs a package with tests", pkg)
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
	src, err := os.ReadFile(m.file)
	if err != nil {
		return "", err
	}
	mutated := filepath.Join(dir, filepath.Base(m.file))
	if err := os.WriteFile(mutated, m.apply(src), 0o666); err != nil {
		return "", err
	}
	ov, _ := json.Marshal(map[string]map[string]string{"Replace": {m.file: mutated}})
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, ov, 0o666); err != nil {
		return "", err
	}
	_, bin, err := buildPackage(pkg, dir, "-overlay="+overlay)
	return bin, err
}

// mutations returns the mistakes the check can plant in file.
func mutations(file string) ([]*mutation, error) {
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
	var ms []*mutation
	add := func(op string, at ast.Node, desc string, es ...edit) {
		ms = append(ms, &mutation{op: op, file: file, line: fset.Position(at.Pos()).Line, desc: desc, edits: es})
	}
	drop := func(n ast.Node) edit { return edit{off(n.Pos()), off(n.End()), ""} }
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
				x, name := method(st, "Lock", "RLock")
				if x == nil {
					continue
				}
				for _, st2 := range n.List[i+1:] {
					if y, uname := method(st2, "Unlock", "RUnlock"); y != nil && text(y) == text(x) && uname == "R"[:len(name)-4]+"Unlock" {
						add("lock", st, fmt.Sprintf("drop the %s and %s of %s", name, uname, text(x)), drop(st), drop(st2))
						break
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "make" && len(n.Args) >= 1 {
				if ct, ok := n.Args[0].(*ast.ChanType); ok {
					switch len(n.Args) {
					case 1:
						add("chan", n, fmt.Sprintf("buffer %s", text(n)), edit{off(n.Rparen), off(n.Rparen), ", 1"})
					case 2:
						if lit, ok := n.Args[1].(*ast.BasicLit); !ok || lit.Value != "0" {
							add("chan", n, fmt.Sprintf("unbuffer %s", text(n)), edit{off(ct.End()), off(n.Args[1].End()), ""})
						}
					}
				}
			}
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sync" && len(n.Args) == 0 {
				add("sync", n, fmt.Sprintf("drop %s, as if it succeeded", text(n)), edit{off(n.Pos()), off(n.End()), "error(nil)"})
			}
		case *ast.ExprStmt:
			if call, ok := n.X.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "close" && len(call.Args) == 1 {
					add("close", n, fmt.Sprintf("drop %s", text(n)), drop(n))
				}
			}
		case *ast.BinaryExpr:
			flip := map[token.Token]string{token.LSS: "<=", token.LEQ: "<", token.GTR: ">=", token.GEQ: ">"}
			if to, ok := flip[n.Op]; ok {
				add("compare", n, fmt.Sprintf("%s to %s", text(n), to), edit{off(n.OpPos), off(n.OpPos) + len(n.Op.String()), to})
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
			add("error", n, fmt.Sprintf("ignore the error in %s", text(b)), edit{off(b.Pos()), off(b.Pos()), "false && "})
		}
		return true
	})
	return ms, nil
}
