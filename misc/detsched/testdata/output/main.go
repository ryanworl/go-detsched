// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Output writes OUTPUT_LINES numbered lines to standard output, with
// a line to standard error after every 1000 of them, and then exits as
// OUTPUT_EXIT says: normally, with that exit status, or with a panic.
package main

import (
	"fmt"
	"os"
	"strconv"
)

func main() {
	n, _ := strconv.Atoi(os.Getenv("OUTPUT_LINES"))
	for i := range n {
		fmt.Printf("line %d\n", i)
		if i%1000 == 999 {
			fmt.Fprintf(os.Stderr, "stderr after line %d\n", i)
		}
	}
	switch exit := os.Getenv("OUTPUT_EXIT"); exit {
	case "":
	case "panic":
		panic("boom")
	default:
		code, _ := strconv.Atoi(exit)
		os.Exit(code)
	}
}
