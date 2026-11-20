// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import (
	"internal/buildcfg"
	"strings"

	"cmd/compile/internal/base"
	"cmd/internal/objabi"
)

// Under GOEXPERIMENT=detsched, the compiler counts ticks and
// synchronization points in source functions (see ssagen's detTick):
// one tick at the entry of every function that counts, unless its
// source body has no calls, loops, or labels (DetLeaf), one on every
// iteration of its loops, and a synchronization point before each of
// its channel operations, go statements and atomic operations. The
// places are defined by the source, not by the compiled code, so a
// build with -N -l counts the same ticks as an optimized one: an
// inlined call counts the tick of its callee's entry, and code inlined
// from a function that does not count counts nothing.

// DetInlCounts reports, for each index of base.Ctxt.InlTree, whether
// the inlined function counts ticks (DetFuncCounts), DetInlPkgCounts
// whether its package does, which decides for the closures in its body,
// and DetInlLeaf whether its body is a leaf (DetLeaf), with no tick at
// its entry.
var DetInlCounts, DetInlPkgCounts, DetInlLeaf []bool

// DetInlLit reports, for each index of base.Ctxt.InlTree, whether the
// inlined function is a function literal. A function literal in its body
// is named as it would be in the function itself, with "." and not
// ".func" before its number (see closureName), as in a build with -l:
// the trace line hashes the names of the functions goroutines run.
var DetInlLit []bool

// DetPkgCounts reports whether functions of the package path count ticks.
// The runtime and the packages it imports do not: they may run where
// preemption is not allowed, and they are deterministic, so leaving them
// out only delays a preemption. That includes the imports that are not
// runtime packages for objabi, such as math/bits: the runtime calls
// them, and they count in neither an optimized build, which inlines
// them, nor a build with -l. Neither do the packages that wrap system
// calls:
//
//   - A goroutine that writes to standard error holds the write lock of
//     os.Stderr's poll.FD, and preempted there, a simulated node that
//     crashes would keep it from every other node for good. Without
//     ticks, nothing preempts the write, which det mode buffers.
//   - Their loops retry system calls interrupted by signals (EINTR),
//     which a debugger causes, so ticks there would make a run under a
//     debugger take another schedule.
//   - The length of the environment, which syscall copies, does not move
//     preemptions.
func DetPkgCounts(path string) bool {
	switch path {
	case "os", "syscall", "internal/poll", "internal/syscall/unix",
		"math/bits", "internal/asan", "internal/msan", "internal/race",
		"internal/runtime/pprof/label", "internal/trace/tracev2":
		return false
	default:
		return !objabi.LookupPkgSpecial(path).Runtime
	}
}

// detFuncPkgCounts reports whether the package whose source holds fn
// counts ticks: for a closure in an inlined body, the inlined function's.
// Neither do the files of detNoCountFiles.
func detFuncPkgCounts(fn *Func) bool {
	pos := base.Ctxt.PosTable.Pos(fn.Pos())
	if i := pos.Base().InliningIndex(); i >= 0 {
		return DetInlPkgCounts[i]
	}
	path := fn.Sym().Pkg.Path
	for _, file := range detNoCountFiles[path] {
		if strings.HasSuffix(pos.Base().Filename(), file) {
			return false
		}
	}
	return DetPkgCounts(path)
}

// detNoCountFiles maps packages to files of theirs where nothing counts
// ticks, because how much work their code does depends on addresses or on
// the set of types in the binary, which differ between an optimized build
// and one with -N -l, or on files that differ between machines. Each file
// declares its own copy of anything it would otherwise call that counts,
// such as atomic.Pointer's methods.
//
//   - The hash tries of internal/sync (sync.Map, and the caches of
//     reflect, encoding/json, encoding/gob and others keyed by types) and
//     of unique: how deep a lookup goes depends on the hash of its key,
//     which for a pointer, such as a reflect.Type, is its address.
//   - reflect's typesByString, which ArrayOf, SliceOf, PointerTo and
//     others call: it binary-searches the types of the binary.
//   - net's parsing of /etc/resolv.conf, which the first lookup of any
//     name reads, even of localhost, and a container's runtime writes
//     for each container, with net's helpers for parsing files and
//     netip's parsing of addresses, which it calls.
var detNoCountFiles = map[string][]string{
	"internal/sync": {"/hashtriemap.go"},
	"unique":        {"/canonmap.go"},
	"reflect":       {"/typesbystring.go"},
	"net":           {"/dnsconfig_unix.go", "/parse.go"},
	"net/netip":     {"/parse.go"},
}

// DetFuncCounts reports whether fn counts ticks and synchronization
// points. Functions the compiler generates, such as method wrappers and
// the closures of go and defer statements, do not: whether a call goes
// through one depends on optimization, such as devirtualization.
func DetFuncCounts(fn *Func) bool {
	if !buildcfg.Experiment.DetSched || base.Flag.CompilingRuntime ||
		fn.Pragma&Nosplit != 0 || fn.Wrapper() || fn.ABIWrapper() ||
		base.Ctxt.PosTable.Pos(fn.Pos()).Base().Filename() == "<autogenerated>" {
		return false
	}
	return detFuncPkgCounts(fn)
}

// DetInlAdd records whether fn, inlined as InlTree index i, counts ticks.
func DetInlAdd(i int, fn *Func) {
	if !buildcfg.Experiment.DetSched {
		return
	}
	if i != len(DetInlCounts) {
		base.Fatalf("detsched: inline index %d, have %d", i, len(DetInlCounts))
	}
	DetInlCounts = append(DetInlCounts, DetFuncCounts(fn))
	DetInlPkgCounts = append(DetInlPkgCounts, !base.Flag.CompilingRuntime && detFuncPkgCounts(fn))
	DetInlLeaf = append(DetInlLeaf, false) // set when the body is read
	DetInlLit = append(DetInlLit, fn.OClosure != nil)
}

// DetLeaf reports whether a function's source body has no calls, loops,
// or labels (which a backward goto needs). Such a function runs a
// bounded amount of its own code, so its entry counts no tick: that
// saves the tick of every inlined call of a small function, such as a
// getter, and stays the same in a build with -N -l, since it is decided
// on the body as the source has it, before inlining. Calls the compiler
// adds later, to the runtime, count no ticks anyway.
func DetLeaf(body Nodes) bool {
	for _, n := range body {
		if Any(n, func(n Node) bool {
			switch n.Op() {
			case OCALLFUNC, OCALLMETH, OCALLINTER, OINLCALL, OFOR, ORANGE, OLABEL, OGOTO:
				return true
			}
			return false
		}) {
			return false
		}
	}
	return true
}
