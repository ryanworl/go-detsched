// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simsharedunlock kills a node whose goroutines contend for a
// mutex shared with another node. Whatever
// the schedule, the other node must get the mutex after the kill: a
// goroutine of the killed node frozen while it unlocks the mutex runs on
// until the mutex is free, and a goroutine waiting for frozen RWMutex
// readers leaves the bubble. TestSimKill explores
// schedules.
package simsharedunlock

import (
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var (
	mu    sync.Mutex
	count int
)

func TestKillUnlocking(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		b.Start(func() {})
		for i := range 30 {
			a.Start(func() {})
			for range 3 {
				a.Go(func() {
					for {
						mu.Lock()
						count++
						mu.Unlock()
					}
				})
			}
			for range 10 + i {
				runtime.Gosched()
			}
			a.Kill()
			done := make(chan bool)
			b.Go(func() {
				mu.Lock()
				count++
				mu.Unlock()
				done <- true
			})
			<-done
		}
	})
}

// TestKillRWMutex kills a node whose goroutines contend for a
// sync.RWMutex of its own. A writer that
// the kill catches after its turn to take the writer mutex came runs on
// as a zombie and then waits for readers that froze: it must not stay
// in the bubble, which would report a deadlock when the test ends.
func TestKillRWMutex(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		for i := range 30 {
			var rw sync.RWMutex
			a.Start(func() {})
			for range 3 {
				a.Go(func() {
					for {
						rw.Lock()
						count++
						rw.Unlock()
					}
				})
				a.Go(func() {
					for {
						rw.RLock()
						runtime.Gosched()
						rw.RUnlock()
					}
				})
			}
			for range 10 + i {
				runtime.Gosched()
			}
			a.Kill()
		}
	})
}

// TestKillChanWait kills a node whose goroutine holds a mutex while it
// waits for a channel that only its own node sends on, then starts a
// goroutine on another node that waits for the mutex, and kills that
// node too. The holder, if the kill caught it runnable, runs on as a
// zombie for the waiter and then waits for the channel for good: it must
// not stay in the bubble, which would report a deadlock when the test
// ends.
func TestKillChanWait(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		for i := range 30 {
			var mu sync.Mutex
			req, resp := make(chan int), make(chan int)
			a.Start(func() {})
			a.Go(func() {
				for {
					v := <-req
					runtime.Gosched()
					resp <- v
				}
			})
			a.Go(func() {
				for {
					mu.Lock()
					runtime.Gosched()
					req <- 1
					<-resp
					mu.Unlock()
				}
			})
			for range 10 + i {
				runtime.Gosched()
			}
			a.Kill()
			b.Start(func() {})
			b.Go(func() {
				mu.Lock()
				count++
				mu.Unlock()
			})
			time.Sleep(time.Second)
			b.Kill()
		}
	})
}
