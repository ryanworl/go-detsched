// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package assert

import "testing"

func TestFailures(t *testing.T) {
	n := Failures()
	Always(true, "assert: test: true", nil)
	Sometimes(false, "assert: test: sometimes", nil)
	Reachable("assert: test: reachable", nil)
	AlwaysOrUnreachable(true, "assert: test: true or unreachable", nil)
	if got := Failures() - n; got != 0 {
		t.Errorf("Failures() rose by %d after passing assertions, want 0", got)
	}
	Always(false, "assert: test: false", nil)
	Unreachable("assert: test: unreachable", nil)
	AlwaysOrUnreachable(false, "assert: test: false or unreachable", nil)
	Always(false, "assert: test: false", nil) // a repeat counts too
	if got := Failures() - n; got != 4 {
		t.Errorf("Failures() rose by %d after 4 failures, want 4", got)
	}
}
