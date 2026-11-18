// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Typecache runs type assertions and type switches on values of many
// dynamic types, whose caches the runtime builds at random calls, in two
// goroutines that hand a channel back and forth, and prints how many
// objects it allocated and the order of a map keyed by pointers, which
// both move when one more cache is built.
package main

import (
	"fmt"
	"hash/fnv"
	"reflect"
	"runtime"
)

func check(v any) int {
	hits := 0
	if _, ok := v.(fmt.Stringer); ok {
		hits++
	}
	switch v.(type) {
	case fmt.Stringer, error:
		hits++
	}
	return hits
}

func main() {
	var vals []any
	for n := range 400 {
		vals = append(vals, reflect.New(reflect.ArrayOf(n, reflect.TypeFor[byte]())).Elem().Interface())
	}
	ping, pong := make(chan int), make(chan int)
	done := make(chan int)
	go func() {
		hits := 0
		for i := range ping {
			hits += check(vals[(i*7)%len(vals)])
			pong <- 0
		}
		done <- hits
	}()
	hits := 0
	for i := range 2000000 {
		hits += check(vals[i%len(vals)])
		if i%64 == 0 {
			ping <- i
			<-pong
		}
	}
	close(ping)
	hits += <-done

	m := map[*int]int{}
	for i := range 200 {
		m[new(int)] = i
	}
	h := fnv.New64a()
	for _, i := range m {
		fmt.Fprint(h, i, ",")
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("hits %d mallocs %d map order %x\n", hits, ms.Mallocs, h.Sum64())
}
