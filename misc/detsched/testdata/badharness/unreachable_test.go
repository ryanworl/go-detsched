// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package badharness

import (
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
)

// TestErrorBranch marks an error branch that no run takes with an Always
// whose condition is false, which no run reaching it is right for: the
// warning must say to use assert.Unreachable.
func TestErrorBranch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		n, err := strconv.Atoi("42")
		if err != nil {
			assert.Always(false, "the number parses", map[string]any{"err": err.Error()})
			return
		}
		assert.Always(n == 42, "the number is 42", nil)
	})
}
