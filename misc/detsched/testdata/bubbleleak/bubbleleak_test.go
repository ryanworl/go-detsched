// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package bubbleleak opens a store whose background goroutines its user
// never stops, so every run ends with synctest's deadlock: the test body
// returns while they are blocked.
package bubbleleak

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type store struct {
	mu   sync.Mutex
	idle *sync.Cond
}

func (s *store) merger() {
	for {
		time.Sleep(time.Millisecond)
		s.mu.Lock()
		s.idle.Wait()
		s.mu.Unlock()
	}
}

func open() *store {
	s := &store{}
	s.idle = sync.NewCond(&s.mu)
	go s.merger()
	go s.merger()
	return s
}

func TestLeak(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		open()
		time.Sleep(5 * time.Millisecond)
	})
}
