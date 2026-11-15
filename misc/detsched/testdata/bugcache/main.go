// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Bugcache has an atomicity violation: get checks the cache and fills it
// under two separate critical sections, so two goroutines can both miss
// and compute the same value. Without perturbation each goroutine runs get
// to completion within its quantum; a perturbed schedule can preempt one
// inside compute, and get panics.
package main

import (
	"fmt"
	"sync"
)

var (
	mu    sync.Mutex
	cache = map[string]int{}
)

func get(key string) int {
	mu.Lock()
	v, ok := cache[key]
	mu.Unlock()
	if ok {
		return v
	}
	v = compute(key)
	mu.Lock()
	defer mu.Unlock()
	if _, dup := cache[key]; dup {
		panic("computed " + key + " twice")
	}
	cache[key] = v
	return v
}

func compute(key string) int {
	h := 0
	for i := range 1000 {
		h = h*31 + int(key[i%len(key)])
	}
	return h
}

func main() {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { get("answer") })
	}
	wg.Wait()
	fmt.Println("ok")
}
