// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package assert states properties of a program that the detsched command
// checks, after the Antithesis SDK's package of the same name. The message
// identifies an assertion, so give each one a distinct, fixed message.
//
// With GOEXPERIMENT=detsched, the runtime reports each failing assertion
// (an Always that is false, an Unreachable that is reached) once at exit,
// with the details of its first failure and how many more times it
// failed, as a line "detsched: assertion failed: MESSAGE DETAILS (and N
// more failures)", by which detsched groups
// failing runs, fails a test during which one failed, so that package
// testing prints its log, and makes a run that would exit with status 0
// exit with status 3. detsched's search also treats the first run that evaluates an
// assertion one way, such as the first Sometimes that is true, as a new
// state to explore from. Without the experiment, failures are printed to
// standard error and nothing else happens.
package assert

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/internal/hook"
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
		if f := hook.AssertFailed; f != nil {
			f(message)
		}
	}
	d := ""
	if failed && len(details) > 0 {
		d = formatDetails(details)
	}
	emit(kind, cond, failed, message, d)
}

// formatDetails formats details as fmt prints a map, map[k1:v1 k2:v2],
// sorted by key, but quotes a value whose text has a space or a character
// that is not printable, so that where each value ends is plain: the
// detsched command groups failures whose only detail is "err" by that
// error, and must tell map[err:x got:y] from an error that reads "x got:y".
func formatDetails(details map[string]any) string {
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	b.WriteString("map[")
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		v := fmt.Sprint(details[k])
		if strings.ContainsFunc(v, func(r rune) bool { return r == ' ' || r == '"' || !unicode.IsPrint(r) }) {
			v = strconv.Quote(v)
		}
		b.WriteString(k + ":" + v)
	}
	b.WriteByte(']')
	return b.String()
}
