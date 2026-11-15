// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Maporder prints a map's iteration order, which depends on the
// runtime's random seed.
package main

import "fmt"

func main() {
	m := map[int]bool{}
	for i := range 32 {
		m[i] = true
	}
	for k := range m {
		fmt.Print(k, " ")
	}
	fmt.Println()
}
