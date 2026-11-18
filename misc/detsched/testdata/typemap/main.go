// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Typemap makes 400 array types with reflect.ArrayOf, which searches the
// binary's types, keeps them in a sync.Map, as the caches of reflect and
// encoding/json are keyed by types, and looks them up many times while
// another goroutine runs. It prints how often that goroutine ran and a
// JSON encoding, so that work that depends on the types' addresses or on
// what types the binary holds moves the schedule.
package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

type point struct {
	X, Y int
	Tags []string
}

func main() {
	stop := make(chan bool)
	spins := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				spins <- n
				return
			default:
				n++
			}
		}
	}()
	var types []reflect.Type
	for n := range 400 {
		types = append(types, reflect.ArrayOf(n, reflect.TypeFor[byte]()))
	}
	var m sync.Map
	for i, t := range types {
		m.Store(t, i)
	}
	sum := 0
	for range 20 {
		for _, t := range types {
			v, _ := m.Load(t)
			sum += v.(int)
		}
	}
	b, err := json.Marshal([]point{{1, 2, []string{"a"}}, {3, 4, nil}})
	if err != nil {
		panic(err)
	}
	stop <- true
	fmt.Printf("sum %d json %s; the other goroutine spun %d times\n", sum, b, <-spins)
}
