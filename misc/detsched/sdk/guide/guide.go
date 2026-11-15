// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package guide lets a program describe its progress to the detsched
// command's search, as FuzzFactory's waypoints and the Antithesis SDK's
// guidance assertions do. The search keeps runs that reach something new
// and explores from them; when a program reports states, only those and
// its assertions count as new, instead of the runtime's generic features.
// So report what distinguishes states that matter: an abstract state of a
// protocol, such as each node's role and how far its log lags, rather
// than raw values that differ in every run.
//
// Without the experiment, the functions do nothing.
package guide

// State reports that the program reached abstract state s in the space
// named name. Each distinct (name, s) is a feature: the first run that
// reports it is new.
func State(name string, s uint64) { state(name, s) }

// Maximize reports a value the search should push up, such as a queue's
// depth or how far behind a replica is. Values are bucketed (each value
// below 32, then each power of two), and a run is new when it reports a
// value in a bucket above every earlier run's.
func Maximize(name string, v int64) { maximize(name, v) }
