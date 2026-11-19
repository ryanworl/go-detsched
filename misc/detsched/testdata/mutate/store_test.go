// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mutate

import (
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

// A disk fails a quarter of its syncs, and counts those that succeed.
type disk struct {
	mu sync.Mutex
	ok int
}

func (d *disk) Sync() error {
	if rand.IntN(4) == 0 {
		return errors.New("EIO")
	}
	d.mu.Lock()
	d.ok++
	d.mu.Unlock()
	return nil
}

// TestStrong runs the code concurrently, with syncs that fail.
func TestStrong(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, l, d, st := NewCounter(1000), &Log{}, &disk{}, &Store{}
		Fanout(4, func(i int) {
			for range 50 {
				c.Add("k", 1)
				l.Commit(d)
				tx := st.Begin()
				c.Get("k") // work in the transaction
				tx.Commit(1)
			}
		})
		assert.Always(c.Get("k") == 200, "the count is the sum of the adds", map[string]any{"got": c.Get("k")})
		assert.Always(st.Value() == 200, "the store's value is the sum of the commits", map[string]any{"got": st.Value()})
		assert.Always(l.Synced() == d.ok, "the log counts the syncs that succeeded", map[string]any{"synced": l.Synced(), "ok": d.ok})
		q, ran := NewQueue(), 0
		for range 10 {
			q.Push(func() { ran++ })
		}
		q.Close()
		assert.Always(ran == 10, "Close waits for the queue's jobs", map[string]any{"ran": ran})
	})
}

type nopSync struct{}

func (nopSync) Sync() error { return nil }

// TestWeak calls the code from one goroutine, and its syncs never fail.
func TestWeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, l := NewCounter(100), &Log{}
		for range 10 {
			c.Add("k", 1)
			l.Commit(nopSync{})
		}
		assert.Always(c.Get("k") == 10, "the count is the sum of the adds", nil)
		q := NewQueue()
		q.Push(func() {})
		q.Close()
	})
}
