// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !ios

// Trampolines for detaslr_darwin.go, called with syscall_rawsyscalln.

#include "textflag.h"

TEXT runtime·detaslr_posix_spawn_trampoline(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)

TEXT runtime·detaslr_posix_spawnattr_init_trampoline(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)

TEXT runtime·detaslr_posix_spawnattr_setflags_trampoline(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)

TEXT runtime·detaslr_dyld_get_image_vmaddr_slide_trampoline(SB),NOSPLIT,$0-0
	JMP	libc__dyld_get_image_vmaddr_slide(SB)

TEXT runtime·detaslr_getpid_trampoline(SB),NOSPLIT,$0-0
	JMP	libc_getpid(SB)
