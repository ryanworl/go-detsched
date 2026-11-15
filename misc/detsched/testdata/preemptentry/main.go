// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Preemptentry is preempted at function entry while GC runs. Run it with
// GODETSEARCH="perturb=off quantum=1", which preempts at every tick. The
// entry tick of a function with a defer must come after the function
// zeroes its named results: the deferred call can read them, so they are
// live at every call, and GC would otherwise find the garbage that dirty
// leaves in the result's stack slot when it shrinks the stack that grow
// enlarged.
package main

import (
	"errors"
	"fmt"
	"runtime"
)

func main() {
	done := make(chan bool)
	go func() {
		for range 500 {
			runtime.GC()
		}
		done <- true
	}()
	n := 0
	for range 20000 {
		grow(64)
		dirty()
		if withDefer() != nil {
			n++
		}
	}
	<-done
	runtime.GC()
	fmt.Println("errors:", n)
}

// grow makes the goroutine's stack large enough for GC to shrink it.
//
//go:noinline
func grow(depth int) int {
	var pad [32]uintptr
	if depth == 0 {
		return len(pad)
	}
	return grow(depth-1) + int(pad[depth%len(pad)])
}

// dirty fills the stack below main's frame with small integers, which GC
// rejects as pointers.
//
//go:noinline
func dirty() uintptr {
	var a [64]uintptr
	for i := range a {
		a[i] = 0x600
	}
	s := uintptr(0)
	for _, v := range a {
		s += v
	}
	return s
}

//go:noinline
func withDefer() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("recovered")
		}
	}()
	return nil
}
