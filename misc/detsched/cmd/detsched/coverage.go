// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Coverage warnings: what a search's runs did not reach, which a search
// with no failures says nothing about otherwise.

// assertPkg is the import path of the SDK's assertions.
const assertPkg = "github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"

// sdkPrefix is the import path prefix of the SDK, whose own assertions
// the catalog leaves out.
const sdkPrefix = "github.com/ryanworl/go-detsched/misc/detsched/sdk/"

// An assertSite is a call of an assertion in the program's source, with
// a constant message.
type assertSite struct {
	kind string // as the runtime reports it, such as "always"
	msg  string // cut at 120 bytes, as the runtime keeps it
	pos  string // file:line
	fn   string // the top-level function it is in, or ""
	no   bool   // an Always whose condition is the constant false
}

// assertKinds are the runtime's names of the assertion functions that
// a catalog lists. An Unreachable no run reaches is as it should be.
var assertKinds = map[string]string{
	"Always":              "always",
	"AlwaysOrUnreachable": "always-or-unreachable",
	"Sometimes":           "sometimes",
	"Reachable":           "reachable",
}

// catalogAsserts lists the assertion calls with a constant message in the
// packages of pkg's module that its test binary links, the SDK's own
// aside, run in dir with env.
func catalogAsserts(pkg, dir string, env []string) ([]assertSite, error) {
	list := exec.Command(goCmd(), "list", "-deps", "-test", "-f",
		`{{if and .Module .Module.Main}}{{.ImportPath}}{{"\t"}}{{.Dir}}{{"\t"}}{{join .GoFiles " "}} {{join .TestGoFiles " "}} {{join .XTestGoFiles " "}}{{end}}`, pkg)
	list.Dir, list.Env = dir, env
	b, err := list.Output()
	if err != nil {
		return nil, err
	}
	files := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 || strings.HasPrefix(f[0], sdkPrefix) {
			continue
		}
		for _, name := range strings.Fields(f[2]) {
			files[filepath.Join(f[1], name)] = true
		}
	}
	var sites []assertSite
	fset := token.NewFileSet()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		local := ""
		for _, im := range file.Imports {
			if p, _ := strconv.Unquote(im.Path.Value); p == assertPkg {
				local = "assert"
				if im.Name != nil {
					local = im.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		fn := ""
		ast.Inspect(file, func(n ast.Node) bool {
			if d, ok := n.(*ast.FuncDecl); ok {
				fn = d.Name.Name
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			kind := assertKinds[sel.Sel.Name]
			if !ok || x.Name != local || kind == "" {
				return true
			}
			arg := 1
			if kind == "reachable" {
				arg = 0
			}
			if len(call.Args) <= arg {
				return true
			}
			msg, ok := constString(call.Args[arg])
			if !ok {
				return true
			}
			if len(msg) > 120 {
				msg = msg[:120]
			}
			p := fset.Position(call.Pos())
			no := false
			if id, ok := call.Args[0].(*ast.Ident); ok && kind == "always" {
				no = id.Name == "false"
			}
			sites = append(sites, assertSite{kind: kind, msg: msg, pos: fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line), fn: fn, no: no})
			return true
		})
	}
	return sites, nil
}

// runSites returns the sites of the tests that -run selects: those in a
// test function whose name matches run, as go test matches top-level
// tests. With -run, a site in another function may belong to a test that
// does not run, so it is left out.
func runSites(sites []assertSite, run string) []assertSite {
	if run == "" {
		return sites
	}
	top, _, _ := strings.Cut(run, "/")
	re, err := regexp.Compile(top)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(sites, func(a assertSite) bool {
		return !strings.HasPrefix(a.fn, "Test") || !re.MatchString(a.fn)
	})
}

// constString returns the value of a string literal, or of a sum of them.
func constString(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			return s, err == nil
		}
	case *ast.ParenExpr:
		return constString(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			x, ok1 := constString(e.X)
			y, ok2 := constString(e.Y)
			return x + y, ok1 && ok2
		}
	}
	return "", false
}

// The SDK's reports of the faults a run could inject, and those it could
// not, with why: "sim: can inject: fault: SITE" or "...: nemesis: KIND",
// and "sim: cannot inject: nemesis: KIND: WHY".
// buggify.Disable reports each site it turns off as "buggify: disabled:
// SITE".
const (
	canInject    = "sim: can inject: "
	cannotInject = "sim: cannot inject: "
	disabledSite = "buggify: disabled: "
)

// injectedFaults returns the faults of faultsOf(out) that are faults,
// leaving out the nemesis's reports of a run's shape and its restores.
func injectedFaults(out []byte) []string {
	return slices.DeleteFunc(faultsOf(out), func(f string) bool {
		return strings.HasPrefix(f, "nemesis: a ") || f == "nemesis: restore" || f == "nemesis: overlapping faults"
	})
}

// faultName returns the name by which the SDK reports that it can inject
// fault f, as faultsOf returns it: "fault: SITE", or "nemesis: KIND".
func faultName(f string) string {
	rest, ok := strings.CutPrefix(f, "nemesis: ")
	if !ok {
		return f
	}
	kind, _, _ := strings.Cut(rest, " ")
	if kind == "clock" {
		kind = "clockskip"
	}
	return "nemesis: " + kind
}

// countFaults counts a run's faults for the coverage warnings. s.mu must
// be held.
func (s *search) countFaults(out []byte) {
	set := false
	for _, m := range assertCounts.FindAllSubmatch(out, -1) {
		set = set || string(m[3]) == setupAssert && string(m[2]) != "0"
	}
	if !set {
		return
	}
	s.simRuns++
	injected := injectedFaults(out)
	if len(injected) == 0 {
		s.noFaultRuns++
		if slices.Contains(faultsOf(out), "nemesis: a run without faults") {
			s.calmRuns++
		}
	}
	for _, f := range injected {
		s.injected[faultName(f)] = true
	}
}

// coverage returns the coverage warnings, as fallbacks. s.mu must be held.
func (s *search) coverage() []fallback {
	if s.runs < 20 {
		return nil
	}
	var fs []fallback
	add := func(anchor, what, cost, fix string) { fs = append(fs, fallback{anchor, what, cost, fix}) }
	list := func(xs []string) string {
		if len(xs) > 5 {
			xs = append(xs[:5:5], fmt.Sprintf("and %d more", len(xs)-5))
		}
		return strings.Join(xs, ", ")
	}

	// Assertions no run reached, and Sometimes assertions that never held.
	var unreached, never []string
	pos := map[string]string{} // the first call of each assertion
	for _, a := range s.catalog {
		k := a.kind + " " + a.msg
		if _, ok := pos[k]; ok {
			continue
		}
		pos[k] = a.pos
		if s.asserts[k] == nil {
			fix := ""
			if a.no {
				fix = "; for a branch no run should reach, use assert.Unreachable"
			}
			unreached = append(unreached, fmt.Sprintf("%s %q (%s%s)", a.kind, a.msg, a.pos, fix))
		}
	}
	for _, c := range slices.SortedFunc(maps.Values(s.asserts), func(a, b *assertCount) int { return strings.Compare(a.msg, b.msg) }) {
		if c.kind != "sometimes" || c.held > 0 || c.msg == setupAssert || slices.ContainsFunc(sdkAsserts, func(p string) bool { return strings.HasPrefix(c.msg, p) }) {
			continue
		}
		where := ""
		if p := pos["sometimes "+c.msg]; p != "" {
			where = p + ", "
		}
		never = append(never, fmt.Sprintf("%q (%sreached in %d runs)", c.msg, where, c.runs))
	}
	if len(unreached) > 0 {
		add("assertions-never-reached", fmt.Sprintf("no run reached %d of the program's %d assertions: %s", len(unreached), len(s.catalog), list(unreached)),
			"what they check was never checked, and the states they mark never reached",
			"make the workload reach them (more operations, clients, or virtual time), or delete dead ones")
	}
	if len(never) > 0 {
		add("sometimes-never-true", fmt.Sprintf("Sometimes assertions that never held (%d): %s", len(never), list(never)),
			"the search never reached the states they mark, so it never tested what follows them",
			"make the workload reach those states, or check that the condition can hold")
	}

	// Runs that ended before any fault.
	if s.simRuns >= 20 && 4*(s.noFaultRuns-s.calmRuns) >= s.simRuns {
		what := fmt.Sprintf("in %d%% of the %d runs that set up a simulation, no fault was injected before the run ended", 100*s.noFaultRuns/s.simRuns, s.simRuns)
		if s.calmRuns > 0 {
			what += fmt.Sprintf(" (%d%% drew a run without faults)", 100*s.calmRuns/s.simRuns)
		}
		add("runs-end-before-faults", what,
			"those runs tested only the fault-free path",
			"keep the workload going longer after setup, with more operations: buggify faults fire on a small share of operations, and a Nemesis may start with a calm of up to 30s")
	}

	// Faults the program could get and never did, and those it cannot.
	var idle, cannot []string
	why := map[string][]string{}
	for _, c := range s.asserts {
		switch {
		case c.kind != "reachable" || c.held == 0:
		case strings.HasPrefix(c.msg, canInject):
			f := strings.TrimPrefix(c.msg, canInject)
			off := s.asserts["reachable "+disabledSite+strings.TrimPrefix(f, "fault: ")]
			if !s.injected[f] && c.held >= 20 && (off == nil || off.held == 0) {
				idle = append(idle, fmt.Sprintf("%s (possible in %d runs)", strings.TrimPrefix(f, "fault: "), c.held))
			}
		case strings.HasPrefix(c.msg, cannotInject):
			rest := strings.TrimPrefix(c.msg, cannotInject)
			if i := strings.LastIndex(rest, ": "); i > 0 && !s.injected[rest[:i]] {
				why[rest[i+2:]] = append(why[rest[i+2:]], strings.TrimPrefix(rest[:i], "nemesis: "))
			}
		}
	}
	for _, w := range slices.Sorted(maps.Keys(why)) {
		slices.Sort(why[w])
		cannot = append(cannot, fmt.Sprintf("nemesis %s (%s)", strings.Join(why[w], ", "), w))
	}
	slices.Sort(idle)
	if len(idle) > 0 || len(cannot) > 0 {
		var parts []string
		if len(idle) > 0 {
			parts = append(parts, "faults no run injected: "+list(idle))
		}
		if len(cannot) > 0 {
			parts = append(parts, "faults the harness cannot get: "+strings.Join(cannot, "; "))
		}
		add("faults-never-injected", strings.Join(parts, "; "),
			"the program was never tested under them",
			"give runs more time in storms, or fix what keeps a fault out; turn off the faults the program does not claim to survive with NemesisConfig.Disable or buggify.Disable")
	}
	return fs
}

// catalogEnv is the environment catalogAsserts lists the packages in: the
// one buildPackage builds them in.
func catalogEnv() []string {
	exp := "detsched"
	if e := os.Getenv("GOEXPERIMENT"); e != "" {
		exp = e + ",detsched"
	}
	return append(os.Environ(), "GOEXPERIMENT="+exp, "CGO_ENABLED=0")
}
