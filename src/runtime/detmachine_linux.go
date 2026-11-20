// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"internal/runtime/syscall/linux"
	"unsafe"
)

// detAuxv returns the value of auxiliary vector entry tag that det mode
// gives the process instead of val: the HWCAP words with only the bits of
// detHWCapKeep, so that code that chooses by them, in the standard
// library or not, takes the same paths on every machine (see
// detmachine.go). Called by sysauxv, before anything reads the vector.
func detAuxv(tag, val uintptr) uintptr {
	if detHWCapKeep == 0 {
		return val
	}
	switch tag {
	case _AT_HWCAP:
		detHWCap.set, detHWCap.hwcap = true, val
		return val & detHWCapKeep
	case _AT_HWCAP2:
		detHWCap.hwcap2 = val
		return 0
	case detAT_HWCAP3, detAT_HWCAP4:
		return 0
	}
	return val
}

// More tags of the auxiliary vector, for detAuxvTags.
const (
	detAT_PHDR              = 3
	detAT_PHENT             = 4
	detAT_PHNUM             = 5
	detAT_BASE              = 7
	detAT_FLAGS             = 8
	detAT_ENTRY             = 9
	detAT_UID               = 11
	detAT_EUID              = 12
	detAT_GID               = 13
	detAT_EGID              = 14
	detAT_CLKTCK            = 17
	detAT_RSEQ_FEATURE_SIZE = 27
	detAT_RSEQ_ALIGN        = 28
	detAT_HWCAP3            = 29
	detAT_HWCAP4            = 30
	detAT_EXECFN            = 31
	detAT_SYSINFO_EHDR      = 33
	detAT_MINSIGSTKSZ       = 51
)

// detAuxvTags is the auxiliary vector that det mode shows the program,
// in this order: each kernel adds entries to the vector (Linux 6.3
// AT_RSEQ_*, and later AT_HWCAP3 and AT_HWCAP4), and code that ranges
// over it, as golang.org/x/sys/cpu does on arm64 to find AT_HWCAP, counts
// a tick for each, so a vector as long as the kernel's would run another
// schedule on another kernel. A tag the kernel did not give has the
// value 0; tags not listed, which only some architectures have, are
// left out.
var detAuxvTags = [...]uintptr{
	detAT_SYSINFO_EHDR, detAT_MINSIGSTKSZ, _AT_HWCAP, _AT_PAGESZ, detAT_CLKTCK,
	detAT_PHDR, detAT_PHENT, detAT_PHNUM, detAT_BASE, detAT_FLAGS, detAT_ENTRY,
	detAT_UID, detAT_EUID, detAT_GID, detAT_EGID, _AT_SECURE, _AT_RANDOM,
	_AT_HWCAP2, detAT_RSEQ_FEATURE_SIZE, detAT_RSEQ_ALIGN, detAT_HWCAP3,
	detAT_HWCAP4, detAT_EXECFN, _AT_PLATFORM,
}

// detAuxvBuf holds the vector det mode shows, with its AT_NULL entry.
var detAuxvBuf [2*len(detAuxvTags) + 2]uintptr

// detMinSigStkSz is AT_MINSIGSTKSZ in det mode. The kernel's value is the
// size of its signal frame, which holds the vector registers, and so
// grows with the SVE or SME vector length on arm64 and the XSAVE area on
// x86: code that read it would learn of the features det mode hides.
// This is more than any of those frames needs.
const detMinSigStkSz = 16384

// detAuxvCanon returns the vector of detAuxvTags with the values of the
// kernel's vector kv (as detAuxv masked them), and detMinSigStkSz, for
// getAuxv.
func detAuxvCanon(kv []uintptr) []uintptr {
	for i, tag := range detAuxvTags {
		detAuxvBuf[2*i] = tag
		if tag == detAT_MINSIGSTKSZ {
			detAuxvBuf[2*i+1] = detMinSigStkSz
			continue
		}
		for j := 0; j+1 < len(kv); j += 2 {
			if kv[j] == tag {
				detAuxvBuf[2*i+1] = kv[j+1]
				break
			}
		}
	}
	return detAuxvBuf[: 2*len(detAuxvTags) : 2*len(detAuxvTags)]
}

// detKernelBuf holds the kernel's release, as the kernel's uname gives
// it, for the machine ID.
var detKernelBuf [6][65]byte

// detKernelRelease returns the kernel's release, which det mode's uname
// hides (see detMachineSyscall).
func detKernelRelease() string {
	const sysUname = 63*goarch.IsAmd64 + 160*goarch.IsArm64
	if sysUname == 0 {
		return ""
	}
	if detKernelBuf[2][0] == 0 {
		linux.Syscall6(sysUname, uintptr(unsafe.Pointer(&detKernelBuf)), 0, 0, 0, 0, 0)
	}
	n := 0
	for n < len(detKernelBuf[2]) && detKernelBuf[2][n] != 0 {
		n++
	}
	return slicebytetostringtmp(&detKernelBuf[2][0], n)
}

// detNofileMax returns the hard limit of open files.
func detNofileMax() (uint64, bool) {
	const sysPrlimit64 = 302*goarch.IsAmd64 + 261*goarch.IsArm64
	const rlimitNofile = 7
	var lim [2]uint64
	if sysPrlimit64 == 0 {
		return 0, false
	}
	if _, _, e := linux.Syscall6(sysPrlimit64, 0, rlimitNofile, 0, uintptr(unsafe.Pointer(&lim)), 0, 0); e != 0 {
		return 0, false
	}
	return lim[1], true
}
