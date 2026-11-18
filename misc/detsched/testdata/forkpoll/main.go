// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forkpoll keeps a pipe registered with the network poller open in the
// middle of its run, then a regular file, and fork mode must refuse to
// fork while either is open.
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	for range 50 {
		runtime.Gosched()
	}
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	for range 50 {
		runtime.Gosched()
	}
	r.Close()
	w.Close()
	for range 50 {
		runtime.Gosched()
	}
	f, err := os.CreateTemp("", "forkpoll")
	if err != nil {
		panic(err)
	}
	defer os.Remove(f.Name())
	for range 50 {
		runtime.Gosched()
	}
	f.Close()
	for range 50 {
		runtime.Gosched()
	}
	fmt.Println("done")
}
