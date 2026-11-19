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
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Fault attribution: which faults a failure needs. In the background,
// after its replay reproduced it, a group's first run is run again from
// its last branch point (from setup's end for a run without one) with
// fresh seeds, once with every fault, then with one kind of fault off at
// a time (GODETSEARCH's faultoff, which the SDK's fault sites read). A
// kind without which the variants stop failing is one the failure needs.
// Faults before the branch point stay in every variant.

const (
	attribVariants = 8 // variants of a run, each run with every fault and with each kind off
	attribKinds    = 6 // the most kinds of fault turned off, those in the most variants first
	attribMinFails = 3 // the variants with every fault that must fail for a verdict
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
	at := uint64(1)
	if s.setup {
		at = s.stop
	}
	s.mu.Unlock()
	branch, trace := r.stopped()
	// The variants keep the tape up to its last branch point, and its
	// steps, and branch there with other seeds.
	prefix, k := "", ""
	if r.tape != "" {
		i := strings.LastIndexByte(r.tape, ',')
		f := strings.Split(r.tape[i+1:], ":")
		prefix, at = r.tape[:i+1], atou(f[0])
		if len(f) == 3 {
			k = ":" + f[2]
		}
	}
	// A failure too rare there for a verdict is tried from later
	// switches of the first run too, halfway to its failure and then
	// three quarters of the way, adding a branch point to its tape.
	ats := []uint64{at}
	end := atou(firstFailSwitch(r.output))
	if m := switchesField.FindStringSubmatch(trace); end == 0 && m != nil {
		end = atou(m[1])
	}
	for _, q := range []uint64{2, 4} {
		if a := end - (end-at)/q; end > at+1 && a > ats[len(ats)-1] {
			ats = append(ats, a)
		}
	}
	// run runs variant v with the faults in off turned off past the
	// branch point, and reports whether it failed as g's runs do and
	// which faults it injected.
	run := func(v int, off string) (bool, []string, error) {
		seed := mix(r.seed, uint64(g.index), uint64(v))
		// feedback=1, for the reports of the faults it injected.
		fields := slices.Concat([]string{fmt.Sprintf("tape=%s%d:%d%s", prefix, at, seed, k), "feedback=1"}, yieldField(r.yield), lim.fields)
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
		var fs []string
		for _, f := range injectedFaults(v2.output) {
			if n := faultName(f); !slices.Contains(fs, n) {
				fs = append(fs, n)
			}
		}
		return v2.key() == g.key, fs, nil
	}
	var (
		base   int            // failing variants
		in     map[string]int // the variants each kind fired in
		before []string       // the faults before the branch point
		where  string
	)
	for i, a := range ats {
		if i > 0 {
			prefix, k = r.tape+",", ""
			if r.tape == "" {
				prefix = ""
			}
		}
		at = a
		where = "its start"
		switch {
		case i > 0:
			where = fmt.Sprintf("switch %d, %d%% of the way to its failure", at, 100*(at-ats[0])/(end-ats[0]))
		case r.tape != "":
			where = fmt.Sprintf("its last branch point (switch %d)", at)
		case at > 1:
			where = fmt.Sprintf("setup's end (switch %d)", at)
		}
		// A variant with every fault off past the branch point still
		// injects those before it.
		var err error
		if _, before, err = run(0, "*"); err != nil {
			return "", err
		}
		base, in = 0, map[string]int{}
		for v := range attribVariants {
			failed, fs, err := run(v, "")
			if err != nil {
				return "", err
			}
			if failed {
				base++
			}
			for _, f := range fs {
				in[f]++
			}
		}
		if base >= attribMinFails {
			break
		}
	}
	if base < attribMinFails {
		return fmt.Sprintf("not found: %d of %d variants of its first run, reseeded at %s, failed this way, too few to tell", base, attribVariants, where), nil
	}
	if len(in) == 0 {
		return fmt.Sprintf("none: %d of %d variants of its first run, reseeded at %s, failed this way, and they injected no faults", base, attribVariants, where), nil
	}
	kinds := slices.SortedFunc(maps.Keys(in), func(a, b string) int { return cmp.Or(in[b]-in[a], strings.Compare(a, b)) })
	if len(kinds) > attribKinds {
		kinds = kinds[:attribKinds]
	}
	// The kinds the variants fail without as often as with them are not
	// needed, and those without which they fail a third as often or less
	// are needed. One that fired before the branch point stays in there,
	// so that turning it off after the branch point may not be enough.
	var buckets [4][]string // needs, fails less without, not needed, and not needed after the reseed
	for _, f := range kinds {
		fails := 0
		for v := range attribVariants {
			failed, _, err := run(v, faultOffName(f))
			if err != nil {
				return "", err
			}
			if failed {
				fails++
			}
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
		buckets[b] = append(buckets[b], fmt.Sprintf("%s (%d of %d", faultLabel(f), fails, attribVariants))
	}
	var parts []string
	for b, name := range []string{"needs", "fails less without", "not needed", "not needed after the reseed, but fired before it too"} {
		if len(buckets[b]) == 0 {
			continue
		}
		ks := buckets[b]
		if len(parts) == 0 {
			ks[0] += " fail without it"
		}
		parts = append(parts, name+": "+strings.Join(ks, "), ")+")")
	}
	return strings.Join(parts, "; ") + fmt.Sprintf("; %d of %d fail with every fault, in variants of its first run reseeded at %s", base, attribVariants, where), nil
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

// firstFailLine is the runtime's report of where a run's first assertion
// failed.
var firstFailLine = regexp.MustCompile(`(?m)^detsched: the first assertion failed at t\S+ s(\d+) `)

// firstFailSwitch returns the switch at which a run's first assertion
// failed, or "".
func firstFailSwitch(out []byte) string {
	if m := firstFailLine.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// mix returns a branch seed for variant v of group g's run of seed:
// splitmix64 of the three, below the runtime's detBranchGoesOn bit.
func mix(seed, g, v uint64) uint64 {
	h := seed*0x9e3779b97f4a7c15 ^ g<<32 ^ v
	h = (h ^ h>>30) * 0xbf58476d1ce4e5b9
	h = (h ^ h>>27) * 0x94d049bb133111eb
	return (h ^ h>>31) >> 1
}
