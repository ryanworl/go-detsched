// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Assertkinds fails one of two assertions, each with details, in most
// runs: the first many times, and in a few of its runs a third, rarer one
// too, as a symptom of the first. In others it fails a check, as a compose
// check fails, with only an error for details, in one of two ways.
package main

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

func main() {
	n := rand.IntN(100)
	switch {
	case n < 50:
		for i := range 500 {
			assert.Always(false, "the first invariant holds", map[string]any{"i": i, "n": n})
		}
		if n < 5 {
			assert.Always(false, "the rare symptom is absent", map[string]any{"n": n})
		}
	case n < 80:
		assert.Always(false, "the second invariant holds", map[string]any{"n": n})
	case n < 90:
		err := fmt.Errorf("node %d has not applied index %d", n%3, n*7)
		if n%2 == 0 {
			err = errors.New("two leaders in one term")
		}
		assert.Always(false, "the check succeeds", map[string]any{"err": err.Error()})
	}
}
