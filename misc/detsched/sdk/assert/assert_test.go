// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package assert

import "testing"

func TestFormatDetails(t *testing.T) {
	for _, c := range []struct {
		details map[string]any
		want    string
	}{
		{map[string]any{"err": "two leaders in one term"}, `map[err:"two leaders in one term"]`},
		{map[string]any{"want": 5, "err": nil, "got": "v3 v4"}, `map[err:<nil> got:"v3 v4" want:5]`},
		{map[string]any{"key": "w1-k3", "addr": "10.0.0.1:80"}, `map[addr:10.0.0.1:80 key:w1-k3]`},
		{map[string]any{"err": "line 1\nline 2"}, `map[err:"line 1\nline 2"]`},
	} {
		if got := formatDetails(c.details); got != c.want {
			t.Errorf("formatDetails(%v) = %s, want %s", c.details, got, c.want)
		}
	}
}

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
