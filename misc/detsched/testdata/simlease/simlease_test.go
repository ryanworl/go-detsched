// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simlease has a planted bug that only a slow fsync finds. Two
// clients take turns holding a lease from a lease service. A client that
// gets the lease records it on its disk, with fsync, and then counts the
// lease's term from when the fsync returned instead of from when it asked
// for the lease. It stops using the lease a safety margin before the term
// it counted ends, which covers every fsync of a healthy disk, but a sync
// that stalls for longer than the margin makes it use the lease after the
// service has given it to the other client. With SIMLEASE_NODISK=1 the
// run has none of the slow-disk faults (sim.disk.slowsync and the
// nemesis's FaultDisk), and it always passes.
package simlease

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

const (
	term   = time.Second            // how long the service grants a lease for
	margin = 250 * time.Millisecond // how early a client stops using it
)

func TestLease(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	noDisk := os.Getenv("SIMLEASE_NODISK") != ""
	if noDisk {
		buggify.Disable("sim.disk.slowsync")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()

		// The lease service, which both clients reach, and what each
		// client believes.
		var mu sync.Mutex
		var holder string
		var expiry time.Time
		believed := map[string]time.Time{}
		acquire := func(who string) bool {
			mu.Lock()
			defer mu.Unlock()
			if holder != "" && time.Now().Before(expiry) {
				return false
			}
			holder, expiry = who, time.Now().Add(term)
			return true
		}
		use := func(who string, until time.Time) {
			mu.Lock()
			defer mu.Unlock()
			for other, u := range believed {
				if other != who && time.Now().Before(u) {
					t.Errorf("%s uses the lease while %s believes it holds it for %v more", who, other, u.Sub(time.Now()))
				}
			}
			believed[who] = until
		}

		client := func(name string) func() {
			return func() {
				f, _ := os.Create("/lease")
				for epoch := 1; ; epoch++ {
					for !acquire(name) {
						time.Sleep(10 * time.Millisecond)
					}
					fmt.Fprintf(f, "%s holds the lease, epoch %d\n", name, epoch)
					f.Sync()
					// The bug: the term started when the service granted
					// the lease, before the fsync.
					until := time.Now().Add(term - margin)
					use(name, until)
					time.Sleep(time.Until(until))
				}
			}
		}
		w.AddNode("a", "10.0.0.1").Start(client("a"))
		w.AddNode("b", "10.0.0.2").Start(client("b"))

		if !noDisk {
			// Only disk faults: the clients share the service's mutex,
			// which a paused or crashed client must not hold.
			nm := w.NewNemesis(sim.NemesisConfig{
				Disable: []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultKill, sim.FaultKillProcess, sim.FaultStop, sim.FaultPause, sim.FaultThrottle, sim.FaultClockSkip},
			})
			nm.Start()
			defer nm.Stop()
		}
		time.Sleep(time.Minute)
	})
}
