// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package assert states properties of a program that the detsched command
// checks, after the Antithesis SDK's package of the same name. The message
// identifies an assertion, so give each one a distinct, fixed message.
//
// With GOEXPERIMENT=detsched, the runtime reports every failure (an Always
// that is false, an Unreachable that is reached) at exit, as a line
// "detsched: assertion failed: MESSAGE DETAILS", by which detsched groups
// failing runs, fails a test during which one failed, so that package
// testing prints its log, and makes a run that would exit with status 0
// exit with status 3. detsched's search also treats the first run that evaluates an
// assertion one way, such as the first Sometimes that is true, as a new
// state to explore from. Without the experiment, failures are printed to
// standard error and nothing else happens.
package assert

import (
	"fmt"
	"sync/atomic"
)

// Kinds of assertion, as the runtime numbers them (runtime/detfeed.go).
const (
	always = iota
	alwaysOrUnreachable
	sometimes
	reachable
	unreachable
)

// Always asserts that cond is true every time this is evaluated.
func Always(cond bool, message string, details map[string]any) {
	report(always, cond, message, details)
}

// AlwaysOrUnreachable asserts that cond is true every time this is
// evaluated, without requiring that it ever is.
func AlwaysOrUnreachable(cond bool, message string, details map[string]any) {
	report(alwaysOrUnreachable, cond, message, details)
}

// Sometimes asserts that cond is true at least once over a search. A
// search treats the first time it is true as a new state to explore from.
func Sometimes(cond bool, message string, details map[string]any) {
	report(sometimes, cond, message, details)
}

// Reachable asserts that this is evaluated at least once over a search.
func Reachable(message string, details map[string]any) {
	report(reachable, true, message, details)
}

// Unreachable asserts that this is never evaluated.
func Unreachable(message string, details map[string]any) {
	report(unreachable, true, message, details)
}

var failures atomic.Int64

// Failures returns the number of assertion failures so far, so that a
// program can print what explains them, such as a history of its run,
// after any failure, including those of packages it uses.
func Failures() int {
	return int(failures.Load())
}

func report(kind int, cond bool, message string, details map[string]any) {
	failed := kind == unreachable || !cond && (kind == always || kind == alwaysOrUnreachable)
	if failed {
		failures.Add(1)
	}
	d := ""
	if failed && len(details) > 0 {
		d = fmt.Sprint(details) // fmt prints maps sorted by key
	}
	emit(kind, cond, failed, message, d)
}
