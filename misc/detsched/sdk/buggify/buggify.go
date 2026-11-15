// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package buggify marks places in a program where a GOEXPERIMENT=detsched
// run may inject a fault, after FoundationDB's BUGGIFY macro.
//
//	if buggify.On("raft.slow-fsync") {
//		time.Sleep(time.Second)
//	}
//
// Each run enables a random quarter of the sites, identified by name, and
// an enabled site fires a quarter of the times it is evaluated (On), or
// with the given probability (OnChance). Enabling only some sites in each
// run is swarm testing: faults that would mask each other in a run with all
// of them are tried separately, and over many runs every combination comes
// up.
//
// The choices come from the runtime's fault stream, which GODETSEED seeds.
// A run that detsched's search forks from the middle of another reseeds it
// and picks a new set of enabled sites, so it explores faults as a fresh
// seed would. Disable turns sites off. Without the experiment, no site is
// ever enabled.
package buggify

import (
	"sync"
	"sync/atomic"
)

// The sites that Disable, Enable, and Force name. The simulation
// evaluates sites in its system call handler, on the system stack, which
// may run while a goroutine is in the middle of Force; so the sets are
// never modified, only replaced, and readers load them atomically.
var (
	off, optIn, forced siteSet
	setMu              sync.Mutex // serializes the replacements
)

type siteSet struct {
	p atomic.Pointer[map[string]bool]
}

func (s *siteSet) has(site string) bool {
	m := s.p.Load()
	return m != nil && (*m)[site]
}

// set replaces s with a copy in which each of sites is on or off.
func (s *siteSet) set(on bool, sites ...string) {
	setMu.Lock()
	defer setMu.Unlock()
	m := map[string]bool{}
	if old := s.p.Load(); old != nil {
		for k := range *old {
			m[k] = true
		}
	}
	for _, site := range sites {
		if on {
			m[site] = true
		} else {
			delete(m, site)
		}
	}
	s.p.Store(&m)
}

// Force makes site fire every time it is evaluated, even if disabled, or,
// with on false, stops forcing it: for a test of how a program handles one
// particular fault.
func Force(site string, on bool) {
	if !supported {
		return
	}
	forced.set(on, site)
}

// Enable allows the given opt-in sites, which OptIn checks: faults that
// few programs claim to survive, such as silent data corruption.
func Enable(sites ...string) { optIn.set(true, sites...) }

// OptIn is Active for a site that is never enabled unless a program
// allows it with Enable.
func OptIn(site string) bool {
	return forced.has(site) || optIn.has(site) && Active(site)
}

// Disable turns the given sites off in every run, for a test that does not
// want those faults, such as a failing fsync in a program that is meant to
// stop on one. Call it before the sites are evaluated.
func Disable(sites ...string) { off.set(true, sites...) }

// On reports whether to inject the fault at site: the site is enabled in
// this run, and fires with probability 1/4.
func On(site string) bool {
	return forced.has(site) || Active(site) && runtime_rand()&3 == 0
}

// OnChance reports whether to inject the fault at site: the site is enabled
// in this run, and fires with probability p.
func OnChance(site string, p float64) bool {
	return forced.has(site) || Active(site) && float64(runtime_rand()>>11)/(1<<53) < p
}

// Active reports whether site is enabled in this run, which a quarter of
// the sites are.
func Active(site string) bool {
	if forced.has(site) {
		return true
	}
	if !supported || off.has(site) {
		return false
	}
	// FNV-1a of the name, mixed with the epoch, which changes at each
	// branch point.
	h := uint64(14695981039346656037)
	for i := 0; i < len(site); i++ {
		h ^= uint64(site[i])
		h *= 1099511628211
	}
	h ^= runtime_epoch()
	h = (h ^ h>>30) * 0xbf58476d1ce4e5b9 // splitmix64's finalizer
	h = (h ^ h>>27) * 0x94d049bb133111eb
	h ^= h >> 31
	return h&3 == 0
}

// Uint64 returns a number from the fault stream, for choosing how a fault
// manifests.
func Uint64() uint64 { return runtime_rand() }
