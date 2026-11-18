// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"fmt"
	"runtime/debug"
	"testing"
)

// TestPanicKind checks that a panic's kind names the function that
// panicked, past the runtime's frames and the frames above the panic that
// took the stack, and the first line of the message with numbers masked.
func TestPanicKind(t *testing.T) {
	var kind string
	n := 5
	func() {
		defer func() { kind = PanicKind(fmt.Sprint(recover())+"\nsecond line 7", debug.Stack()) }()
		var s []int
		_ = s[n]
	}()
	want := "runtime error: index out of range [N] with length N in github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.TestPanicKind.func1"
	if kind != want {
		t.Errorf("PanicKind = %q, want %q", kind, want)
	}
}
