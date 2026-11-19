// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Runaway runs. Every run after the first that passed runs with limits
// set from the most the passing runs went, so that a run that would not
// finish stops at a defined switch, which its replay line stops at too,
// and fails as one that did not finish (see the runtime's detrunaway.go).

// An extent is how far a run went: its switches, the virtual time its
// bubbles' clocks went, the most switches it made while they stayed
// put, and the most bytes a GC found live.
type extent struct {
	switches, stuck, heap uint64
	time                  time.Duration
}

// extentOf returns the extent a run reports at its end.
func extentOf(out []byte) (extent, bool) {
	t, e := switchesField.FindSubmatch(traceLine.Find(out)), extentLine.FindSubmatch(out)
	if t == nil || e == nil {
		return extent{}, false
	}
	return extent{
		switches: atou(string(t[1])),
		time:     time.Duration(atou(string(e[1]))),
		stuck:    atou(string(e[2])),
		heap:     atou(string(e[3])),
	}, true
}

// grow raises each measure of x to that of y.
func (x *extent) grow(y extent) {
	x.switches, x.stuck, x.heap, x.time = max(x.switches, y.switches), max(x.stuck, y.stuck), max(x.heap, y.heap), max(x.time, y.time)
}

// Limits, as multiples of the longest passing run, each at least its
// multiple of a floor, for programs whose passing runs are short.
const (
	runMultiple   = 20 // switches, virtual time, and memory
	stuckMultiple = 10 // switches while virtual time stays put

	switchesFloor = 50_000
	stuckFloor    = 10_000
	timeFloor     = 3 * time.Minute
	heapFloor     = 16 << 20
)

// limits are the limits a run ran with, and the extent of the passing
// runs they were set from.
type limits struct {
	fields []string // GODETSEARCH fields, or none before any run passed
	base   extent
}

// limits returns the limits of a run that starts now. s.mu must be held.
func (s *search) limits() limits {
	if s.passed == 0 {
		return limits{}
	}
	l := s.longest
	return limits{base: l, fields: []string{
		fmt.Sprintf("maxswitches=%d", runMultiple*max(l.switches, switchesFloor)),
		fmt.Sprintf("maxstuck=%d", stuckMultiple*max(l.stuck, stuckFloor)),
		fmt.Sprintf("maxtime=%d", runMultiple*max(l.time, timeFloor)),
		fmt.Sprintf("maxheap=%d", runMultiple*max(l.heap, heapFloor)),
	}}
}

// value returns the limit on what a run that stopped for kind, as the
// runtime names it, measured, or 0 if it ran without one.
func (l limits) value(kind string) uint64 {
	name := map[string]string{"switches": "maxswitches=", "livelock": "maxstuck=", "time": "maxtime=", "memory": "maxheap="}[kind]
	for _, f := range l.fields {
		if v, ok := strings.CutPrefix(f, name); ok && name != "" {
			return atou(v)
		}
	}
	return 0
}

// rerunRaised runs r again, from its start, when it stopped at a limit
// that the runs that passed since it started have raised, and returns
// the run to record. The first runs to pass are the short ones, so a run
// that started then has limits that a run of the program's usual length
// can pass.
func (s *search) rerunRaised(ctx context.Context, r *result) (*result, error) {
	for !r.timedOut && !r.outOfMem {
		m := stopLine.FindSubmatch(r.output)
		if m == nil {
			break
		}
		s.mu.Lock()
		lim := s.limits()
		s.mu.Unlock()
		if old := r.limits.value(string(m[1])); old == 0 || lim.value(string(m[1])) <= old {
			break
		}
		var fields []string
		if r.tape != "" {
			fields = append(fields, "tape="+r.tape)
		}
		cmd := s.plain(r.seed)
		cmd.Env = addSearch(cmd.Env, slices.Concat(fields, yieldField(r.yield), lim.fields)...)
		again, err := s.exec(ctx, cmd)
		if errors.Is(err, errCanceled) {
			break
		}
		if err != nil {
			return nil, err
		}
		again.seed, again.tape, again.yield, again.limits = r.seed, r.tape, r.yield, lim
		r = again
	}
	return r, nil
}

// field returns the field of the limit the run with output out stopped
// at, which its replay needs to stop at the same switch, or none. For a
// debug line, which may run the build without optimizations, whose heap
// differs, a run stopped for its memory stops at the same switch instead
// (stopat=), as both builds make the same switches.
func (l limits) field(out []byte, debug bool) []string {
	m := stopLine.FindSubmatch(out)
	if m == nil {
		return nil
	}
	if debug && string(m[1]) == "memory" {
		return []string{"stopat=" + string(m[3])}
	}
	name := map[string]string{"switches": "maxswitches=", "livelock": "maxstuck=", "time": "maxtime=", "memory": "maxheap="}[string(m[1])]
	for _, f := range l.fields {
		if name != "" && strings.HasPrefix(f, name) {
			return []string{f}
		}
	}
	return nil
}

// explain returns the signature of a run stopped at a limit, from the
// runtime's report: which limit (kind), the value that passed it, and
// the switch and virtual time it stopped at.
func (l limits) explain(kind string, v uint64, at, t string) string {
	times := func(v, base uint64) string { return fmt.Sprintf("%d×", v/max(base, 1)) }
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		t = fmt.Sprintf("%.3fs", f) // one shape, which failureKey masks the same
	}
	switch kind {
	case "livelock":
		return fmt.Sprintf("did not finish: livelock at t=%s: virtual time stayed put for %d switches (not counting preemptions), where the most a passing run made was %d", t, v, l.base.stuck)
	case "switches":
		return fmt.Sprintf("did not finish: ran %s the longest passing run: %d switches, at t=%s", times(v, l.base.switches), v, t)
	case "time":
		if l.base.time == 0 {
			return fmt.Sprintf("did not finish: ran %.3fs of virtual time, where no passing run's clock moved, at switch %s", float64(v)/1e9, at)
		}
		return fmt.Sprintf("did not finish: ran %s the longest passing run: %.3fs of virtual time, at switch %s", times(v, uint64(l.base.time)), float64(v)/1e9, at)
	case "memory":
		return fmt.Sprintf("did not finish: memory: %d MB live after a GC, %s the most a passing run held, at t=%s", v>>20, times(v, l.base.heap), t)
	case "stop":
		return fmt.Sprintf("did not finish: still running at switch %s, where the run it replays was stopped", at)
	}
	return "did not finish: " + kind
}

// lastSwitchLine matches a line of the runtime's report of the goroutines
// a run's last switches went to: the switches, the goroutines, and where
// they are, innermost first, separated by " < ".
var lastSwitchLine = regexp.MustCompile(`^detsched: \t(\d+) (goroutine \d+|exited goroutines|other goroutines),?(?: (.*))?$`)

// whereOf returns the goroutines the last switches of a run that did not
// finish went to, the 3 with the most, with where each is, or "": the
// innermost function of a package in mods, the code under test, or else
// the innermost function. Goroutines that have exited are named by the
// functions they started in.
func whereOf(out []byte, mods []string) string {
	m := lastSwitches.FindSubmatch(out)
	if m == nil {
		return ""
	}
	type part struct {
		n int
		p string
	}
	var parts []part
	for _, line := range strings.Split(strings.TrimSuffix(string(m[2]), "\n"), "\n") {
		l := lastSwitchLine.FindStringSubmatch(line)
		if l == nil {
			continue
		}
		p := l[1] + " to " + l[2]
		switch {
		case l[2] == "exited goroutines":
			var fns []string
			for _, fn := range strings.Split(strings.TrimPrefix(l[3], "started in "), ", ") {
				fns = append(fns, fn[strings.LastIndex(fn, "/")+1:])
			}
			p += ", started in " + strings.Join(fns, ", ")
		case strings.HasPrefix(l[3], "("):
			p += " " + l[3] // (exited)
		case strings.HasSuffix(l[3], " (where it started)"):
			p += " in " + l[3][strings.LastIndex(l[3], "/")+1:]
		case l[3] != "":
			frames := strings.Split(l[3], " < ")
			f := frames[0]
			for _, fr := range frames {
				if inModules(fr, mods) {
					f = fr
					break
				}
			}
			fn, pos, _ := strings.Cut(f, " ")
			p += " in " + fn[strings.LastIndex(fn, "/")+1:]
			if pos != "" {
				p += " (" + filepath.Base(pos) + ")"
			}
		}
		n, _ := strconv.Atoi(l[1])
		parts = append(parts, part{n, p})
	}
	slices.SortStableFunc(parts, func(a, b part) int { return b.n - a.n })
	var top []string
	for i, p := range parts {
		if i < 3 {
			top = append(top, p.p)
		}
	}
	return fmt.Sprintf("of its last %s switches, %s", m[1], strings.Join(top, ", "))
}

// inModules reports whether frame, "FUNC FILE:LINE", is in a package of
// one of mods, but the SDK.
func inModules(frame string, mods []string) bool {
	fn, _, _ := strings.Cut(frame, " ")
	pkg := fn
	if i := strings.LastIndexByte(pkg, '/'); i >= 0 {
		if j := strings.IndexByte(pkg[i:], '.'); j >= 0 {
			pkg = pkg[:i+j]
		}
	} else if j := strings.IndexByte(pkg, '.'); j >= 0 {
		pkg = pkg[:j]
	}
	if strings.HasPrefix(pkg+"/", sdkPrefix) {
		return false
	}
	for _, m := range mods {
		if pkg == m || strings.HasPrefix(pkg, m+"/") {
			return true
		}
	}
	return false
}

// codeModules returns the modules of the code under test in bin: its main
// module and those it replaces with a local directory.
func codeModules(bin string) []string {
	bi, err := buildinfo.ReadFile(bin)
	if err != nil {
		return nil
	}
	mods := []string{bi.Main.Path}
	for _, d := range bi.Deps {
		if d.Replace != nil && d.Replace.Version == "" {
			mods = append(mods, d.Path)
		}
	}
	return mods
}
