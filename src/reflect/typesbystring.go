// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect

import (
	"internal/abi"
	"unsafe"
)

// Under GOEXPERIMENT=detsched, nothing in this file counts ticks or
// synchronization points (see detFuncPkgCounts in
// cmd/compile/internal/ir): how many steps the binary search of the
// binary's types takes depends on what types the binary holds, which
// differs between an optimized build and one with -N -l.

// typesByString returns all known types whose elements have
// the given string representation.
// It may be empty (no known types with that string) or may have
// multiple elements (multiple types with that string).
//
// typesByString should be an internal detail,
// but widely used packages access it using linkname.
// Notable members of the hall of shame include:
//   - github.com/aristanetworks/goarista
//   - fortio.org/log
//
// Do not remove or change the type signature.
// See go.dev/issue/67401.
//
//go:linkname typesByString
func typesByString(s string) []*abi.Type {
	first, rest := compiledTypelinks()
	var ret []*abi.Type

	searchTypes := func(types []*abi.Type) {
		// We are looking for the first index i where the string becomes >= s.
		// This is a copy of sort.Search, with f(h) replaced by (*typ[h].String() >= s).
		i, j := 0, len(types)
		for i < j {
			h := int(uint(i+j) >> 1) // avoid overflow when computing h
			// i ≤ h < j
			if !(typeString(types[h]) >= s) {
				i = h + 1 // preserves f(i-1) == false
			} else {
				j = h // preserves f(j) == true
			}
		}
		// i == j, f(i-1) == false, and f(j) (= f(i)) == true  =>  answer is i.

		// Having found the first, linear scan forward to find the last.
		// We could do a second binary search, but the caller is going
		// to do a linear scan anyway.
		for j := i; j < len(types); j++ {
			typ := types[j]
			if typeString(typ) != s {
				break
			}
			ret = append(ret, typ)
		}
	}

	searchTypes(first)
	for _, r := range rest {
		searchTypes(r)
	}

	return ret
}

// typeString is the String method of t's rtype, declared here so that it
// counts no ticks either.
func typeString(t *abi.Type) string {
	s := abi.Name{Bytes: (*byte)(resolveNameOff(unsafe.Pointer(t), int32(t.Str)))}.Name()
	if t.TFlag&abi.TFlagExtraStar != 0 {
		return s[1:]
	}
	return s
}
