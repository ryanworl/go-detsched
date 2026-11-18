// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Buildinfo reads its build info, as a library does to learn its own
// version, many times while another goroutine runs, and prints the build
// settings it sees, so that a build that sees other settings, or parses a
// longer string, runs another schedule or prints something else.
package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

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
	var settings []string
	for range 300 {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			panic("no build info")
		}
		settings = settings[:0]
		for _, s := range info.Settings {
			settings = append(settings, s.Key)
		}
	}
	stop <- true
	fmt.Printf("settings %s; the other goroutine spun %d times\n", strings.Join(settings, " "), <-spins)
}
