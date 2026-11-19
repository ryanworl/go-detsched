// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package envscan ranges over its environment while two goroutines race
// to update a counter, so that the number of variables decides where
// they are preempted, and the race loses an update on some seeds.
package envscan

import (
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

func TestScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		count := 0
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() {
				for range 10 {
					c := count
					n := 0
					for _, e := range os.Environ() {
						n += len(strings.Fields(e))
					}
					count = c + 1
					time.Sleep(time.Duration(n%3) * time.Millisecond)
				}
			})
		}
		wg.Wait()
		assert.Always(count == 20, "no update is lost", map[string]any{"count": count})
	})
}
