// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !ios

// Trampolines for fork mode on darwin; see detfork_darwin.go. Each takes
// a detForkCall in R0 and stores the libc function's result in r and,
// if the result is -1, errno in errno.

#include "go_asm.h"
#include "textflag.h"

TEXT runtime·detfork_fork_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	BL	libc_fork(SB)
	MOVW	R0, R0	// pid_t: sign-extend
	MOVD	$0, R1
	MOVD	$-1, R2
	CMP	R0, R2
	BNE	ok
	BL	libc_error(SB)
	MOVW	(R0), R1
	MOVD	$-1, R0
ok:
	MOVD	R0, detForkCall_r(R19)
	MOVD	R1, detForkCall_errno(R19)
	RET

TEXT runtime·detfork_wait4_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	MOVW	detForkCall_a1(R19), R0	// pid
	MOVD	detForkCall_a2(R19), R1	// status
	MOVW	detForkCall_a3(R19), R2	// options
	MOVD	$0, R3			// rusage
	BL	libc_wait4(SB)
	MOVW	R0, R0
	MOVD	$0, R1
	MOVD	$-1, R2
	CMP	R0, R2
	BNE	ok
	BL	libc_error(SB)
	MOVW	(R0), R1
	MOVD	$-1, R0
ok:
	MOVD	R0, detForkCall_r(R19)
	MOVD	R1, detForkCall_errno(R19)
	RET

TEXT runtime·detfork_dup2_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	MOVW	detForkCall_a1(R19), R0	// oldfd
	MOVW	detForkCall_a2(R19), R1	// newfd
	BL	libc_dup2(SB)
	MOVW	R0, R0
	MOVD	$0, R1
	MOVD	$-1, R2
	CMP	R0, R2
	BNE	ok
	BL	libc_error(SB)
	MOVW	(R0), R1
	MOVD	$-1, R0
ok:
	MOVD	R0, detForkCall_r(R19)
	MOVD	R1, detForkCall_errno(R19)
	RET

TEXT runtime·detfork_recvmsg_trampoline(SB),NOSPLIT,$0
	MOVD	R0, R19
	MOVW	detForkCall_a1(R19), R0	// fd
	MOVD	detForkCall_a2(R19), R1	// msg
	MOVW	detForkCall_a3(R19), R2	// flags
	BL	libc_recvmsg(SB)
	MOVD	$0, R1
	MOVD	$-1, R2
	CMP	R0, R2
	BNE	ok
	BL	libc_error(SB)
	MOVW	(R0), R1
	MOVD	$-1, R0
ok:
	MOVD	R0, detForkCall_r(R19)
	MOVD	R1, detForkCall_errno(R19)
	RET
