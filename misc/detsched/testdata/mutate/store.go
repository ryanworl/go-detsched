// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package mutate is code under test for detsched -check, with two
// harnesses: one that tests it concurrently and with failing syncs, and
// one that calls it from one goroutine and never sees an error, whose
// clean searches prove nothing (TestCheck).
package mutate

import (
	"sync"
	"sync/atomic"
)

// A Counter counts by key, keeping each count within a limit.
type Counter struct {
	mu    sync.Mutex
	n     map[string]int
	limit func(int) int
	adds  atomic.Int64 // for stats, read without mu
}

// NewCounter returns a Counter whose counts stop at max.
func NewCounter(max int) *Counter {
	return &Counter{n: map[string]int{}, limit: func(v int) int {
		if v > max {
			return max
		}
		return v
	}}
}

// Add adds d to k's count.
func (c *Counter) Add(k string, d int) {
	c.mu.Lock()
	v := c.n[k]
	c.adds.Add(1)
	c.n[k] = c.limit(v + d)
	c.mu.Unlock()
}

// Adds returns the number of calls of Add.
func (c *Counter) Adds() int64 { return c.adds.Load() }

// Get returns k's count.
func (c *Counter) Get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}

// A Syncer makes what was written durable.
type Syncer interface{ Sync() error }

// A Log counts the records it made durable.
type Log struct {
	mu     sync.Mutex
	synced int
}

// Commit makes a record durable with w, and counts it once w's Sync
// succeeded.
func (l *Log) Commit(w Syncer) error {
	if err := w.Sync(); err != nil {
		return err
	}
	l.mu.Lock()
	l.synced++
	l.mu.Unlock()
	return nil
}

// Synced returns the records the log made durable.
func (l *Log) Synced() int {
	mu := &l.mu
	mu.Lock()
	defer mu.Unlock()
	return l.synced
}

// Fanout runs f on each of n workers and returns when all are done.
func Fanout(n int, f func(i int)) {
	done := make(chan int)
	for i := 0; i < n; i++ {
		go func() {
			f(i)
			done <- i
		}()
	}
	for range n {
		<-done
	}
}

// A Queue runs jobs in order on one goroutine.
type Queue struct {
	jobs chan func()
	done chan struct{}
}

func NewQueue() *Queue {
	q := &Queue{jobs: make(chan func(), 4), done: make(chan struct{})}
	go func() {
		for j := range q.jobs {
			j()
		}
		close(q.done)
	}()
	return q
}

// Push adds a job.
func (q *Queue) Push(j func()) { q.jobs <- j }

// Close waits for the jobs pushed before it.
func (q *Queue) Close() {
	close(q.jobs)
	<-q.done
}
