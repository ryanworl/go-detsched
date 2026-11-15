// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Assertall fails several assertions, each with its own details, from
// goroutines that take turns.
package main

import (
	"fmt"
	"sync"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

func main() {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			mu.Lock()
			defer mu.Unlock()
			assert.Always(false, "every item is valid", map[string]any{"item": i})
		})
		wg.Wait()
	}
	assert.Always(true, "a passing assertion", nil)
	assert.Unreachable("the end is unreachable", map[string]any{"items": 3})
	fmt.Println("done")
}
