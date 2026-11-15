// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Spin runs two goroutines in tight loops, one of them without function
// calls, and prints the order in which they took turns. With one P, they
// take turns only when the runtime preempts them.
package main

import (
	"fmt"
	"sync"
)

const chunks = 200

var (
	mu   sync.Mutex
	log  []byte
	sink int
)

func note(id byte) {
	mu.Lock()
	if len(log) == 0 || log[len(log)-1] != id {
		log = append(log, id)
	}
	mu.Unlock()
}

//go:noinline
func add(x, i int) int { return x + i }

func main() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // loop body without calls
		defer wg.Done()
		x := 0
		for c := range chunks {
			for i := range 1 << 18 {
				x += i ^ c
			}
			note('L')
		}
		sink += x
	}()
	go func() { // loop body with calls
		defer wg.Done()
		x := 0
		for range chunks {
			for i := range 1 << 16 {
				x = add(x, i)
			}
			note('C')
		}
		sink += x
	}()
	wg.Wait()
	fmt.Printf("%s\n", log)
}
