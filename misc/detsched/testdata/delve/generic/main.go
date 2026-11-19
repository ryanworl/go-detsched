// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Generic has a method of a generic type called through an interface, so
// that the binary has its wrapper, (*lease[string]).take, and its body,
// (*lease[go.shape.string]).take, at the same line, for the tests of the
// bundled Delve in the directory above: a breakpoint on the method names
// both with their type arguments.
package main

import "fmt"

type taker interface{ take(int) bool }

type lease[K comparable] struct {
	held map[K]int
	key  K
}

//go:noinline
func (l *lease[K]) take(n int) bool {
	l.held[l.key] += n
	return l.held[l.key] < 3
}

func main() {
	var t taker = &lease[string]{held: map[string]int{}, key: "a"}
	for i := 0; i < 3; i++ {
		fmt.Println(t.take(1))
	}
}
