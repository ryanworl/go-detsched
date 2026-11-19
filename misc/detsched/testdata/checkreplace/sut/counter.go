// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sut is code under test in its own module, which the harness
// module next to it tests through a replace directive, as harness.md
// recommends (TestMutationCheck).
package sut

import (
	"sync"
	"sync/atomic"
)

// A Counter counts by key.
type Counter struct {
	mu   sync.Mutex
	n    map[string]int
	adds atomic.Int64 // for stats, read without mu
}

func NewCounter() *Counter { return &Counter{n: map[string]int{}} }

// Add adds d to k's count.
func (c *Counter) Add(k string, d int) {
	c.mu.Lock()
	v := c.n[k]
	c.adds.Add(1)
	c.n[k] = v + d
	c.mu.Unlock()
}

// Get returns k's count.
func (c *Counter) Get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}
