// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Promoted has the shapes of two tracepoint traps, for the tests of the
// bundled Delve in the directory above. As in bbolt's freelist, the
// method AddReadonlyTXID of shared is promoted through the embedded
// struct into array and hashMap, and calls through the interface go
// through the compiler's wrappers (*array).AddReadonlyTXID and
// (*hashMap).AddReadonlyTXID, into which it is inlined. And work, called
// once by each of several goroutines, and depth, which calls itself, are
// for conditional tracepoints whose returns must follow their entries.
package main

import (
	"fmt"
	"sync"
)

type shared struct {
	readonly []int
}

func (s *shared) AddReadonlyTXID(id int) {
	s.readonly = append(s.readonly, id)
}

type array struct {
	*shared
	ids []int
}

type hashMap struct {
	*shared
	m map[int]int
}

type freelist interface {
	AddReadonlyTXID(id int)
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
	fls := []freelist{&array{shared: &shared{}}, &hashMap{shared: &shared{}}}
	for id := 1; id <= 4; id++ {
		fls[id%2].AddReadonlyTXID(id)
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
