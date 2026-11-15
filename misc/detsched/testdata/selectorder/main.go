// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Selectorder prints which of two ready channels each select picks,
// which depends on the runtime's random seed.
package main

import "fmt"

func main() {
	a, b := make(chan int, 1), make(chan int, 1)
	for range 64 {
		a <- 0
		b <- 0
		select {
		case <-a:
			fmt.Print("a")
			<-b
		case <-b:
			fmt.Print("b")
			<-a
		}
	}
	fmt.Println()
}
