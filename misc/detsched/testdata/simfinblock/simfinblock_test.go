// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simfinblock sets a finalizer in a synctest bubble that blocks
// for good on a channel of the bubble. The bubble must report a deadlock
// that names it, as it does any other goroutine of the bubble.
// TestSimPrograms runs it.
package simfinblock

import (
	"runtime"
	"testing"
	"testing/synctest"
)

type stream struct{ buf [64]byte }

//go:noinline
func open(never chan struct{}) {
	runtime.SetFinalizer(new(stream), func(*stream) { <-never })
}

func TestBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		open(make(chan struct{}))
		runtime.GC()
	})
}
