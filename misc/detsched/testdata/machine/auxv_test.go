// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package machine

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
	"slices"
	_ "unsafe"
)

//go:linkname runtime_getAuxv runtime.getAuxv
func runtime_getAuxv() []uintptr

// auxvSum ranges over the auxiliary vector, as golang.org/x/sys/cpu does
// through getAuxv to find AT_HWCAP on arm64, and over /proc/self/auxv, as
// github.com/klauspost/cpuid does, and does work for each HWCAP bit set,
// as a library that chooses code by them would. With MACHINE_AUXV set it
// prints the vector's tags, and whether the file holds the same vector.
var auxvSum = func() int {
	const atHWCap, atHWCap2 = 16, 26
	n := 0
	a := runtime_getAuxv()
	var tags, vec []uint64
	for len(a) >= 2 {
		tag, val := a[0], a[1]
		a = a[2:]
		tags = append(tags, uint64(tag))
		vec = append(vec, uint64(tag), uint64(val))
		if tag == atHWCap || tag == atHWCap2 {
			n += hwcapSum(uint64(val))
		}
	}
	b, err := os.ReadFile("/proc/self/auxv")
	var file []uint64
	for err == nil && len(b) >= 16 {
		tag, val := binary.LittleEndian.Uint64(b), binary.LittleEndian.Uint64(b[8:])
		b = b[16:]
		if tag == 0 {
			break
		}
		file = append(file, tag, val)
		if tag == atHWCap || tag == atHWCap2 {
			n += hwcapSum(val)
		}
	}
	if os.Getenv("MACHINE_AUXV") != "" {
		fmt.Println("auxv tags:", tags)
		fmt.Println("auxv file same:", slices.Equal(vec, file))
	}
	return n
}()

func hwcapSum(v uint64) int {
	n := 0
	for v != 0 {
		n += leaf(bits.TrailingZeros64(v))
		v &= v - 1
	}
	return n
}
