// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"

// func detReadMIDR() uint64
TEXT ·detReadMIDR(SB),NOSPLIT,$0-8
	MRS	MIDR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func detReadDCZID() uint64
TEXT ·detReadDCZID(SB),NOSPLIT,$0-8
	MRS	DCZID_EL0, R0
	MOVD	R0, ret+0(FP)
	RET
