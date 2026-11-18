// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Promoted has the shapes of two tracepoint traps, for the tests of the
// bundled Delve in the directory above. The method Record of base is
// promoted through the embedded struct into list and index, and calls
// through the interface go through the compiler's wrappers
// (*list).Record and (*index).Record, into which it is inlined. And work, called
// once by each of several goroutines, and depth, which calls itself, are
// for conditional tracepoints whose returns must follow their entries.
package main

import (
	"fmt"
	"sync"
)

type base struct {
	ids []int
}

func (s *base) Record(id int) {
	s.ids = append(s.ids, id)
}

type list struct {
	*base
	ids []int
}

type index struct {
	*base
	m map[int]int
}

type recorder interface {
	Record(id int)
}

//go:noinline
func work(i int) int {
	return i * i
}

//go:noinline
func depth(n int) int {
	if n == 0 {
		return 0
	}
	return depth(n-1) + 1
}

func main() {
	rs := []recorder{&list{base: &base{}}, &index{base: &base{}}}
	for id := 1; id <= 4; id++ {
		rs[id%2].Record(id)
	}

	var wg sync.WaitGroup
	sum := 0
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := work(i)
			mu.Lock()
			sum += r
			mu.Unlock()
		}()
	}
	wg.Wait()
	fmt.Println(sum, depth(4))
}
