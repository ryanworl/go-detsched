// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package lifecycle tells the detsched command's search about the phases
// of a program, after the Antithesis SDK's package of the same name.
// Without GOEXPERIMENT=detsched its functions do nothing.
package lifecycle

// SetupComplete reports that the program's setup is done. The runtime
// records the switch of the first call, so that the search starts its runs
// right after it instead of running setup again. The details are not used
// yet.
func SetupComplete(details map[string]any) {
	setupComplete()
}

// Branches returns how many times the run has branched: the search forks
// runs from the middle of others, each with new random choices after the
// branch. A test whose workload has a fixed length should count it from
// the last branch, as package detsched/sdk/compose does, so that every
// branch gets a whole workload after it rather than what was left of its
// parent's. Without the experiment it returns 0.
func Branches() int {
	return branches()
}
