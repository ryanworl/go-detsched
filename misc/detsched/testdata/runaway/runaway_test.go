// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package runaway holds harnesses whose runs do not finish on every
// fourth seed, each its own way, and finish quickly on the others: the
// search must stop them at limits set from the passing runs, and report
// each way as one failure (TestSearchRunaway). TestCompute is the
// control, which only takes longer on those seeds.
package runaway

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// bad reports whether this seed's run does not finish.
func bad() bool {
	seed, _ := strconv.Atoi(os.Getenv("GODETSEED"))
	return seed%4 == 0
}

// TestLivelock retries a busy server without a backoff. The server is
// busy until a timer fires, which needs virtual time to move, which it
// does only when every goroutine waits, which they never do.
func TestLivelock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready atomic.Bool
		time.AfterFunc(10*time.Millisecond, func() { ready.Store(true) })
		req, resp := make(chan int), make(chan bool)
		go func() {
			for range req {
				resp <- ready.Load()
			}
		}()
		if !bad() {
			time.Sleep(20 * time.Millisecond)
		}
		for {
			req <- 1
			if <-resp {
				break
			}
		}
		close(req)
	})
}

// TestBranchPoll polls forever in a branch of the timeline, which a
// composer forks to check recovery, while the timeline passes. The run's
// failure is the branch's, so its replay must run the branch in place.
func TestBranchPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(time.Millisecond)
		if sim.Branch("the branch finishes") {
			for stuck := bad(); stuck; {
				time.Sleep(time.Millisecond)
			}
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	})
}

// TestPoll polls for a flag that a lost signal never sets, so the poll
// goes on forever while virtual time moves.
func TestPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var done atomic.Bool
		go func() {
			time.Sleep(time.Second)
			if !bad() {
				done.Store(true)
			}
		}()
		for !done.Load() {
			time.Sleep(time.Millisecond)
		}
	})
}

// TestHistory keeps every entry it ever made, which grows without bound
// on the seeds where trimming is lost.
func TestHistory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var history [][]byte
		for i := 0; i < 100 || bad(); i++ {
			mu.Lock()
			history = append(history, make([]byte, 1<<20))
			if !bad() && len(history) > 8 {
				history = history[1:]
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	})
}

// TestCompute computes 15 times longer on every fourth seed, without
// letting virtual time move, and passes: a long computation is neither a
// livelock nor a runaway. With a short quantum, its goroutines are
// preempted over 10 times as often as in the passing runs.
func TestCompute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n := 1 << 20
		if bad() {
			n *= 15
		}
		var wg sync.WaitGroup
		sums := make([]uint64, 2)
		for i := range sums {
			wg.Go(func() {
				for j := range n {
					sums[i] = sums[i]*31 + uint64(j)
				}
			})
		}
		wg.Wait()
	})
}

// TestLong always finishes, in 9 times as many switches on seeds 3 to 5
// as on seed 2, and 27 times on later seeds. With 4 runs at a time, seed
// 6 starts once seed 2 has passed, with limits set from it, which seed 6
// passes; once seeds 3 to 5 have passed too, it must finish, and not be
// reported as a runaway.
func TestLong(t *testing.T) {
	seed, _ := strconv.Atoi(os.Getenv("GODETSEED"))
	n := 1 << 18
	switch {
	case seed >= 6:
		n *= 27
	case seed >= 3:
		n *= 9
	}
	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		sums := make([]uint64, 2)
		for i := range sums {
			wg.Go(func() {
				for j := range n {
					sums[i] = sums[i]*31 + uint64(j)
				}
			})
		}
		wg.Wait()
	})
}
