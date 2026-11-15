// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Bugorder has an order violation: a worker reads the configuration that
// main publishes only after starting it. Without perturbation the worker
// first runs when main blocks, so the bug never shows; a perturbed
// schedule can run the worker first, and it panics.
package main

import (
	"fmt"
	"sync/atomic"
)

var config atomic.Pointer[string]

func main() {
	done := make(chan bool)
	go func() {
		if config.Load() == nil {
			panic("worker ran before the configuration was published")
		}
		done <- true
	}()
	config.Store(loadConfig())
	<-done
	fmt.Println("ok")
}

//go:noinline
func loadConfig() *string {
	s := "ready"
	return &s
}
