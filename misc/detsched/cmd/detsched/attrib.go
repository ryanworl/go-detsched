// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Fault attribution: which faults a failure needs. In the background,
// after its replay reproduced it, a group's first run is run again from
// setup's end (from its start for a run without setup) with fresh seeds,
// once with every fault, then with one kind of fault off for the rest of
// the run (GODETSEARCH's faultoff, which the SDK's fault sites read). A
// kind without which the variants stop failing is one the failure needs.
// The variants part at setup's end, not later, as a failure noticed long
// after its cause (a liveness check at a deadline) fails in every variant
// reseeded after the cause, with or without the faults. A rare failure
// gets more variants, up to attribMax.

const (
	attribBatch    = 8  // variants run at a time, and the fewest run
	attribMax      = 64 // the most variants run with every fault, for a rare failure
	attribKinds    = 6  // the most kinds of fault turned off, those in the most variants first
	attribMinFails = 4  // the variants with every fault that must fail for a verdict
)

// attribute finds the faults that g's failure needs, from r, its first
// run, and sets g.attrib. It runs one group at a time.
func (s *search) attribute(ctx context.Context, g *group, r *result) {
	defer s.attribWG.Done()
	s.attribSem <- true
	defer func() { <-s.attribSem }()
	line, err := s.attribution(ctx, g, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case errors.Is(err, errCanceled) || ctx.Err() != nil:
		line = "not found: the search ended first"
	case err != nil:
		line = "not found: " + truncate(err.Error(), 300)
	}
	g.attrib = line
	s.printf("detsched: group %d (%s), faults: %s\n", g.index, g.msg(), line)
}

// attribution returns g's attribution line.
func (s *search) attribution(ctx context.Context, g *group, r *result) (string, error) {
	s.mu.Lock()
	lim := s.limits()
	at, where := uint64(1), "its start"
	if s.setup {
		at = s.stop
		// The run's own setup's end, which seed 1's may not be: a branch
		// point past the run's end would leave every variant the run.
		if m := setupLine.FindSubmatch(r.output); m != nil {
			at = atou(string(m[1])) + 1
		}
		where = fmt.Sprintf("setup's end (switch %d)", at)
	}
	s.mu.Unlock()
	branch, _ := r.stopped()
	// The run's branch points before the variants', which they keep.
	var prefix []string
	last := uint64(0) // the run's last branch point, where faultoff starts in it
	for _, e := range strings.Split(r.tape, ",") {
		if pos, _, ok := strings.Cut(e, ":"); ok {
			if atou(pos) < at {
				prefix = append(prefix, e)
			}
			last = atou(pos)
		}
	}
	// run runs variant v with the faults in off turned off past the
	// branch point, and reports whether it failed as g's runs do and
	// which faults it injected.
	run := func(v int, off string) (bool, []string, error) {
		seed := mix(r.seed, uint64(g.index), uint64(v))
		// feedback=1, for the reports of the faults it injected.
		tape := strings.Join(append(slices.Clone(prefix), fmt.Sprintf("%d:%d", at, seed)), ",")
		fields := slices.Concat([]string{"tape=" + tape, "feedback=1"}, yieldField(r.yield), lim.fields)
		if off != "" {
			fields = append(fields, "faultoff="+off)
		}
		cmd := s.plain(r.seed, "GODETBRANCH="+strconv.Itoa(branch))
		cmd.Env = addSearch(cmd.Env, fields...)
		v2, err := s.exec(ctx, cmd)
		if err != nil {
			return false, nil, err
		}
		v2.limits = lim
		return v2.key() == g.key, kindsIn(v2.output), nil
	}
	// batch runs variants from to to+attribBatch, at most s.workers at a
	// time, and returns how many failed and the faults each injected.
	batch := func(from int, off string) (int, [][]string, error) {
		var (
			wg     sync.WaitGroup
			sem    = make(chan bool, max(s.workers, 1))
			failed = make([]bool, attribBatch)
			faults = make([][]string, attribBatch)
			errs   = make([]error, attribBatch)
		)
		for i := range attribBatch {
			wg.Add(1)
			sem <- true
			go func() {
				defer func() { <-sem; wg.Done() }()
				failed[i], faults[i], errs[i] = run(from+i, off)
			}()
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return 0, nil, err
		}
		n := 0
		for _, f := range failed {
			if f {
				n++
			}
		}
		return n, faults, nil
	}
	// A variant with every fault off still injects those before the
	// branch point.
	_, before, err := run(0, "*")
	if err != nil {
		return "", err
	}
	base, n, in := 0, 0, map[string]int{} // failing variants, variants, and the variants each kind fired in
	for n < attribMax && base < attribMinFails {
		fails, faults, err := batch(n, "")
		if err != nil {
			return "", err
		}
		base, n = base+fails, n+attribBatch
		for _, fs := range faults {
			for _, f := range fs {
				in[f]++
			}
		}
	}
	if base < attribMinFails {
		// Too few variants fail to compare, as for a failure that needs
		// a fault only some branch points enable: turn each kind its
		// first run injected off in that run itself instead, from its
		// last branch point.
		var needs, not []string
		kinds := kindsIn(r.output)
		for _, f := range kinds[:min(len(kinds), attribKinds)] {
			fields := slices.Concat([]string{"feedback=1"}, yieldField(r.yield), lim.fields, []string{"faultoff=" + faultOffName(f)})
			if r.tape != "" {
				fields = append(fields, "tape="+r.tape)
			}
			cmd := s.plain(r.seed, "GODETBRANCH="+strconv.Itoa(branch))
			cmd.Env = addSearch(cmd.Env, fields...)
			v, err := s.exec(ctx, cmd)
			if err != nil {
				return "", err
			}
			v.limits = lim
			if v.key() == g.key {
				not = append(not, faultLabel(f))
			} else {
				needs = append(needs, faultLabel(f))
			}
		}
		from := "from its start"
		if last > 1 {
			from = fmt.Sprintf("from switch %d", last)
		}
		line := fmt.Sprintf("%d of %d variants of its first run, reseeded at %s, failed this way, too few to compare", base, n, where)
		switch {
		case len(needs) > 0:
			line = fmt.Sprintf("needs, in its first run, which passes with it off %s: %s; %s", from, strings.Join(needs, ", "), line)
		case len(not) > 0:
			line = fmt.Sprintf("not found: its first run still fails with each of its faults off %s (%s); %s", from, strings.Join(not, ", "), line)
		default:
			line = "not found: " + line
		}
		return line, nil
	}
	if len(in) == 0 {
		return fmt.Sprintf("none: %d of %d variants of its first run, reseeded at %s, failed this way, and they injected no faults", base, n, where), nil
	}
	kinds := slices.SortedFunc(maps.Keys(in), func(a, b string) int { return cmp.Or(in[b]-in[a], strings.Compare(a, b)) })
	if len(kinds) > attribKinds {
		kinds = kinds[:attribKinds]
	}
	// The kinds the variants fail without as often as with them are not
	// needed, and those without which they fail a third as often or less
	// are needed. A kind is run on the same variants as every fault was,
	// until as many fail without it as make it not needed. One that
	// fired before the branch point stays in there, so that turning it
	// off after it may not be enough.
	var buckets [4][]string // needs, fails less without, not needed, and not needed after the branch point
	for _, f := range kinds {
		fails, ran := 0, 0
		for ran < n && 3*fails < 2*base {
			nf, _, err := batch(ran, faultOffName(f))
			if err != nil {
				return "", err
			}
			fails, ran = fails+nf, ran+attribBatch
		}
		b := 2
		switch {
		case 3*fails <= base:
			b = 0
		case 3*fails < 2*base:
			b = 1
		}
		if b > 0 && slices.Contains(before, f) {
			b = 3
		}
		buckets[b] = append(buckets[b], fmt.Sprintf("%s (%d of %d", faultLabel(f), fails, ran))
	}
	var parts []string
	for b, name := range []string{"needs", "fails less without", "not needed", "not needed after setup's end, but fired before it too"} {
		if len(buckets[b]) == 0 {
			continue
		}
		ks := buckets[b]
		if len(parts) == 0 {
			ks[0] += " fail without it"
		}
		parts = append(parts, name+": "+strings.Join(ks, "), ")+")")
	}
	return strings.Join(parts, "; ") + fmt.Sprintf("; %d of %d fail with every fault, in variants of its first run reseeded at %s", base, n, where), nil
}

// kindsIn returns the kinds of fault a run's output says it injected.
func kindsIn(out []byte) []string {
	var fs []string
	for _, f := range injectedFaults(out) {
		if n := faultName(f); !slices.Contains(fs, n) {
			fs = append(fs, n)
		}
	}
	return fs
}

// faultOffName returns the name GODETSEARCH's faultoff knows fault f by,
// as faultName returns it: SITE, or nemesis:KIND.
func faultOffName(f string) string {
	if k, ok := strings.CutPrefix(f, "nemesis: "); ok {
		return "nemesis:" + k
	}
	return strings.TrimPrefix(f, "fault: ")
}

// faultLabel returns how a summary names fault f: SITE, or nemesis KIND.
func faultLabel(f string) string {
	return strings.Replace(strings.TrimPrefix(f, "fault: "), "nemesis: ", "nemesis ", 1)
}

// mix returns a branch seed for variant v of group g's run of seed:
// splitmix64 of the three, below the runtime's detBranchGoesOn bit.
func mix(seed, g, v uint64) uint64 {
	h := seed*0x9e3779b97f4a7c15 ^ g<<32 ^ v
	h = (h ^ h>>30) * 0xbf58476d1ce4e5b9
	h = (h ^ h>>27) * 0x94d049bb133111eb
	return (h ^ h>>31) >> 1
}
