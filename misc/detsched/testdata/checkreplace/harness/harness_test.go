// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package harness tests example.com/sut, which go.mod replaces with the
// local copy next to it. It has only test files.
package harness

import (
	"sync"
	"testing"
	"testing/synctest"

	"example.com/sut"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

func TestCounter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := sut.NewCounter()
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 50 {
					c.Add("k", 1)
				}
			})
		}
		wg.Wait()
		assert.Always(c.Get("k") == 200, "the count is the sum of the adds", map[string]any{"got": c.Get("k")})
	})
}
