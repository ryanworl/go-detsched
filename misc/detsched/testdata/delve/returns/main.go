// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Returns has functions with the shapes of results Go's internal ABI
// returns in registers, and one it returns on the stack, for the tests of
// the bundled Delve in the directory above: a tracepoint's return lines
// read them from the registers at the function's RET, where optimized
// code's debug information has no location for most of them.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

var banner = strings.Repeat("0123456789", 30)

type node struct{ id int }

type pair struct {
	small int8
	big   int64
}

//go:noinline
func lookup(key string) (string, error) {
	if key == "" {
		return "", errors.New("empty key")
	}
	return key + "-value", nil
}

//go:noinline
func count(n int) (int, error) {
	if n < 0 {
		return 0, os.ErrInvalid
	}
	return n * 3, nil
}

//go:noinline
func find(n int) (*node, bool) {
	if n%2 == 0 {
		return &node{id: n}, true
	}
	return nil, false
}

//go:noinline
func mix(n int) pair {
	return pair{small: int8(n), big: int64(n) << 40}
}

//go:noinline
func ratio(n int) float64 {
	return float64(n) / 4
}

//go:noinline
func span(n int) [2]int {
	return [2]int{n, n + 1}
}

func main() {
	fmt.Println(len(banner))
	v, err := lookup("k1")
	fmt.Println(v, err)
	_, err = lookup("")
	fmt.Println(err)
	c, err := count(7)
	fmt.Println(c, err)
	nd, ok := find(4)
	fmt.Println(nd.id, ok)
	fmt.Println(mix(5), ratio(3), span(9))
}
