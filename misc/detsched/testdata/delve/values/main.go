// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Values has values whose printing the tests of the bundled Delve in the
// directory above check: a buffered channel, closed with values left in
// it, and a field read through an interface field, j.env.err.
package main

import (
	"errors"
	"fmt"
)

type env interface{ Failed() bool }

type session struct {
	err   error
	tries int
}

func (s *session) Failed() bool { return s.err != nil }

type job struct {
	env  env
	name string
}

//go:noinline
func work(j *job, ch chan int) int {
	return len(j.name) + len(ch)
}

func main() {
	ch := make(chan int, 100)
	ch <- 1
	ch <- 2
	ch <- 3
	close(ch)
	j := &job{env: &session{err: errors.New("boom"), tries: 2}, name: "x"}
	fmt.Println(work(j, ch))
}
