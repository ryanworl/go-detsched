// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package lockholds has a lock order that predicts a deadlock that cannot
// happen, and then two goroutines that each lock a pair of their own
// mutexes many times, at the same site as the predicted deadlock's first
// Lock. A run directed at the prediction holds both at that site; holding
// them must not hand the P back and forth between them until the run
// looks like a livelock.
package lockholds

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type pair struct{ a, b sync.Mutex }

//go:noinline
func (p *pair) lockAB() {
	p.a.Lock()
	p.b.Lock()
	p.b.Unlock()
	p.a.Unlock()
}

//go:noinline
func (p *pair) lockBA() {
	p.b.Lock()
	p.a.Lock()
	p.a.Unlock()
	p.b.Unlock()
}

//go:noinline
func serve(wg *sync.WaitGroup, p *pair, n int, start <-chan struct{}, done chan<- struct{}) {
	wg.Go(func() {
		if start != nil {
			<-start
		}
		for range n {
			p.lockAB()
		}
		if done != nil {
			close(done)
		}
	})
}

func TestHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Opposite orders on one pair, one after the other: a predicted
		// deadlock at lockAB's first Lock that no schedule reaches.
		var wg sync.WaitGroup
		p := &pair{}
		step := make(chan struct{})
		serve(&wg, p, 1, nil, step)
		wg.Go(func() {
			<-step
			p.lockBA()
		})
		wg.Wait()

		// Two goroutines, each on its own pair, at the same site.
		start := make(chan struct{})
		for range 2 {
			serve(&wg, &pair{}, 200, start, nil)
		}
		synctest.Wait()
		time.Sleep(time.Millisecond)
		close(start)
		wg.Wait()
	})
}
