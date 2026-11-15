// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Setupbugs runs a concurrent setup phase, eight goroutines passing $SETUP
// messages around a ring, reports that setup is complete, and then runs one
// seeded bug kernel chosen by $BUG: order, cache, atomic, or lockorder. It
// measures how cheaply branching after setup finds bugs that fresh seeds
// find only by running setup again.
package main

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
)

func setup(rounds int) int {
	const nodes = 8
	inbox := make([]chan int, nodes)
	for i := range inbox {
		inbox[i] = make(chan int, 4)
	}
	var wg sync.WaitGroup
	sums := make([]int, nodes)
	for i := range nodes {
		wg.Go(func() {
			state := map[int]int{}
			for r := range rounds {
				if i == 0 {
					inbox[1] <- r
					v := <-inbox[0]
					state[v%64] += mix(v)
					continue
				}
				v := <-inbox[i]
				state[v%64] += mix(v)
				inbox[(i+1)%nodes] <- v + 1
			}
			for _, v := range state {
				sums[i] += v
			}
		})
	}
	wg.Wait()
	t := 0
	for _, s := range sums {
		t += s
	}
	return t
}

// mix stands for the work a node does per message.
func mix(v int) int {
	for range 200 {
		v = v*6364136223846793005 + 1442695040888963407
		v ^= v >> 29
	}
	return v & 0xffff
}

// order
var config atomic.Pointer[string]

func bugOrder() {
	done := make(chan bool)
	go func() {
		if config.Load() == nil {
			panic("worker ran before the configuration was published")
		}
		done <- true
	}()
	config.Store(loadConfig())
	<-done
}

//go:noinline
func loadConfig() *string { s := "ready"; return &s }

// cache
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

func bugCache() {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { get("answer") })
	}
	wg.Wait()
}

// atomic
var (
	done   atomic.Bool
	setups atomic.Int32
)

func setupOnce() {
	if !done.Load() {
		done.Store(true)
		setups.Add(1)
	}
}

func bugAtomic() {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(setupOnce)
	}
	wg.Wait()
	if n := setups.Load(); n != 1 {
		panic(fmt.Sprint("setup ran ", n, " times"))
	}
}

// lockorder
func bugLockOrder() {
	testing.Main(func(string, string) (bool, error) { return true, nil }, []testing.InternalTest{{Name: "TestTransfer", F: func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var a, b sync.Mutex
			var wg sync.WaitGroup
			wg.Go(func() { lockBoth(&a, &b) })
			wg.Go(func() { lockBoth(&b, &a) })
			wg.Wait()
		})
	}}}, nil, nil)
}

func lockBoth(first, second *sync.Mutex) {
	first.Lock()
	defer first.Unlock()
	work()
	second.Lock()
	defer second.Unlock()
	work()
}

func work() int {
	n := 0
	for i := range 100 {
		n += i
	}
	return n
}

func main() {
	rounds, _ := strconv.Atoi(os.Getenv("SETUP"))
	fmt.Println("setup", setup(rounds))
	lifecycle.SetupComplete(nil)
	switch os.Getenv("BUG") {
	case "order":
		bugOrder()
	case "cache":
		bugCache()
	case "atomic":
		bugAtomic()
	case "lockorder":
		bugLockOrder()
	}
	fmt.Println("ok")
}
