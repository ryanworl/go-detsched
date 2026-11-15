// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Forkpoll keeps a pipe registered with the network poller open in the
// middle of its run, which fork mode must refuse to fork.
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
	fmt.Println("done")
}
