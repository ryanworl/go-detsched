// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Stagedbug fails only after three rare events in a row. After a setup
// phase, eight goroutines passing $SETUP messages around a ring, stage a
// runs a check-then-act race between two goroutines; only if both compute
// the value, and a 1 in 4 random choice (standing for an injected fault)
// says so, does stage b run, then stage c, and getting through that one
// too panics. The random choices keep the stages independent: a schedule
// that loses one race tends to lose them all. A search that keeps the runs
// that reached a new stage and branches from them needs only a few tries
// per stage, where fresh runs need all three at once. Each stage reached is
// an assert.Sometimes, unless $NOSDK is set, leaving only the runtime's own
// feedback features to tell.
package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/lifecycle"
)

var sdk = os.Getenv("NOSDK") == ""

// race runs two goroutines that each compute and cache a value, checking
// the cache first in a separate critical section, and reports whether both
// computed it.
func race() bool {
	var (
		mu       sync.Mutex
		cached   bool
		computed int
	)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			mu.Lock()
			ok := cached
			mu.Unlock()
			if ok {
				return
			}
			work()
			mu.Lock()
			computed++
			cached = true
			mu.Unlock()
		})
	}
	wg.Wait()
	return computed == 2
}

func work() int {
	h := 0
	for i := range 1000 {
		h = h*31 + i
	}
	return h
}

// lost reports whether a stage's race was lost and its random choice says
// to go on.
func lost() bool {
	return race() && rand.N(4) == 0
}

func stageA() {
	if lost() {
		if sdk {
			assert.Sometimes(true, "stage a lost its race", nil)
		}
		stageB()
	}
}

func stageB() {
	if lost() {
		if sdk {
			assert.Sometimes(true, "stage b lost its race", nil)
		}
		stageC()
	}
}

func stageC() {
	if lost() {
		panic("all three stages lost their race")
	}
}

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
			for r := range rounds {
				if i == 0 {
					inbox[1] <- r
					sums[i] += <-inbox[0]
					continue
				}
				v := <-inbox[i]
				sums[i] += v
				inbox[(i+1)%nodes] <- v + 1
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

func main() {
	rounds, _ := strconv.Atoi(os.Getenv("SETUP"))
	fmt.Println("setup", setup(rounds))
	lifecycle.SetupComplete(nil)
	stageA()
	fmt.Println("ok")
}
