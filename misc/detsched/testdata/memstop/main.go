// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memstop computes for about half a second of real time, long enough for
// a test to have the search stop it as if memory ran low, and passes.
package main

import "fmt"

func main() {
	x := uint64(1)
	for range 100_000_000 {
		x = x*6364136223846793005 + 1442695040888963407
	}
	fmt.Println("done", x>>60 < 16)
}
