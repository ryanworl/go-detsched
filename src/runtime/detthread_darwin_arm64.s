// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !ios

// Trampolines for detthread_darwin.go, called with libcCall.

#include "textflag.h"

TEXT runtime·detthread_pthread_mach_thread_np_trampoline(SB),NOSPLIT,$0
	MOVD	0(R0), R0	// arg 1 thread
	BL	libc_pthread_mach_thread_np(SB)
	RET

TEXT runtime·detthread_thread_info_trampoline(SB),NOSPLIT,$0
	MOVD	8(R0), R1	// arg 2 flavor
	MOVD	16(R0), R2	// arg 3 info
	MOVD	24(R0), R3	// arg 4 count
	MOVD	0(R0), R0	// arg 1 port
	BL	libc_thread_info(SB)
	RET
