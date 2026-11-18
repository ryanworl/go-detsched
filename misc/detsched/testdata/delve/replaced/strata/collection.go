// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package strata is a module that ../app replaces with this directory,
// for the tests of the bundled Delve's locations in the directory above.
package strata

var sink int

//go:noinline
func Put(k int) int {
	sink += k
	return sink
}

// Set has the shape of two inlined methods that share an address:
// release is inlined into Free at Free's first statement. Drop calls
// release after a check, at an address of its own.
type Set struct{ ids []int }

func (s *Set) Free(id int) { s.release(id) }

func (s *Set) release(id int) { s.ids = append(s.ids[:0], id) }

func (s *Set) Drop(id int) {
	if id < 0 {
		return
	}
	s.release(id)
}

// Visit has a closure in a closure, Visit.func1.1.
func Visit(n int) int {
	total := 0
	Each(1, func(int) {
		Each(n, func(i int) { total += Put(i) })
	})
	return total
}

//go:noinline
func Each(n int, f func(int)) {
	for i := range n {
		f(i)
	}
}
