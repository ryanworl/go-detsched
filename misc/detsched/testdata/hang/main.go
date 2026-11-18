// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Hang waits an hour of real time on every third seed, as a run stuck on
// something outside the program would, and returns at once on the others.
package main

import (
	"os"
	"strconv"
	"time"
)

func main() {
	if seed, _ := strconv.Atoi(os.Getenv("GODETSEED")); seed%3 == 2 {
		time.Sleep(time.Hour)
	}
}
