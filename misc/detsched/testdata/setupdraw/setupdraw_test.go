// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package setupdraw runs a setup whose length depends on the seed, draws
// a number when setup completes, and fails when it drew 0, 1 in 16 runs.
// Runs a search forks from a setup root must each draw their own: a root
// stopped past its setup's end, as at another seed's longer setup, would
// leave every run forked from it with the root's draw.
package setupdraw

import (
	"math/rand/v2"
	"runtime"
	"sync"
	"testing"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
)

// rounds runs n rounds of yields in each of four goroutines.
func rounds(n int) {
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range n {
				runtime.Gosched()
			}
		})
	}
	wg.Wait()
}

func TestDraw(t *testing.T) {
	rounds(1 + rand.IntN(100))
	lifecycle.SetupComplete(nil)
	drew := rand.IntN(16)
	rounds(200) // longer than any setup
	if drew == 0 {
		t.Errorf("drew 0 when setup completed")
	}
}
